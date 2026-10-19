package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

func parseTimeAny(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", value); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("invalid time")
}

func parseCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := []string{}
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) handleImageTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	wanted := parseCSV(r.URL.Query().Get("ids"))
	wantMap := map[string]bool{}
	for _, taskID := range wanted {
		wantMap[taskID] = true
	}
	items := []ImageTask{}
	seen := map[string]bool{}
	for _, task := range s.store.LoadTasks() {
		if task.OwnerID != id.ID && id.Role != "admin" {
			continue
		}
		if len(wantMap) > 0 && !wantMap[task.ID] {
			continue
		}
		items = append(items, projectImageTask(task))
		seen[task.ID] = true
	}
	missing := []string{}
	for _, taskID := range wanted {
		if !seen[taskID] {
			missing = append(missing, taskID)
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing})
}

func projectImageTask(task ImageTask) ImageTask {
	requested := max(1, task.RequestedCount)
	if task.Mode == "edit" {
		requested = 1
	}
	task.RequestedCount = requested
	task.SucceededCount = min(requested, max(0, len(task.Data)))
	if isActiveImageTaskStatus(task.Status) {
		task.Terminal = false
		task.PendingCount = max(0, requested-task.SucceededCount)
		task.FailedCount = 0
	} else {
		task.Terminal = true
		task.PendingCount = 0
		task.FailedCount = max(0, requested-task.SucceededCount)
	}
	if !isActiveImageTaskStatus(task.Status) {
		task.StageCode, task.StageLabel = imageTaskStage(task.Status)
		if task.DurationMS > 0 {
			task.ElapsedMS = task.DurationMS
		}
	} else {
		if task.StageCode == "" || task.StageLabel == "" {
			task.StageCode, task.StageLabel = imageTaskStage(task.Status)
		}
		base := task.CreatedAt
		if task.Status == "running" && task.StartedAt != "" {
			base = task.StartedAt
		}
		task.ElapsedMS = maxInt64(0, time.Since(parseTaskCreatedAt(base)).Milliseconds())
	}
	return task
}

type imageTaskKey struct{ ownerID, id string }

func (s *Server) insertTask(t ImageTask) (ImageTask, bool, error) {
	created := false
	result := t
	err := s.store.UpdateTasks(func(tasks []ImageTask) []ImageTask {
		for _, existing := range tasks {
			if existing.OwnerID == t.OwnerID && existing.ID == t.ID {
				result = existing
				return tasks
			}
		}
		created = true
		return append([]ImageTask{t}, tasks...)
	})
	return result, created && err == nil, err
}

func (s *Server) setTaskCancel(ownerID, id string, cancel context.CancelFunc) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	s.taskCancels[imageTaskKey{ownerID, id}] = cancel
}
func (s *Server) popTaskCancel(ownerID, id string) context.CancelFunc {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	key := imageTaskKey{ownerID, id}
	cancel := s.taskCancels[key]
	delete(s.taskCancels, key)
	return cancel
}

func (s *Server) taskIsActive(ownerID, id string) bool {
	for _, task := range s.store.LoadTasks() {
		if task.OwnerID == ownerID && task.ID == id {
			return isActiveImageTaskStatus(task.Status)
		}
	}
	return false
}
func (s *Server) recoverUnfinishedTasks() error {
	var tasks []ImageTask
	if err := s.store.LoadDocument("image_tasks.json", &tasks); err != nil {
		return fmt.Errorf("读取未完成图片任务失败: %w", err)
	}
	changed := false
	for _, task := range tasks {
		if !isActiveImageTaskStatus(task.Status) {
			continue
		}
		updated, err := s.store.recoverImageTask(task.OwnerID, task.ID)
		if err != nil {
			return fmt.Errorf("恢复图片任务 %s 失败: %w", task.ID, err)
		}
		if !updated {
			continue
		}
		changed = true
	}
	if changed {
		if s.logSvc != nil {
			s.logSvc.add("system", "恢复未完成图片任务", map[string]any{"status": "recovered"})
		}
	}
	return nil
}

func (s *Server) cleanupOldTasks() error {
	days := s.cfg.ImageRetentionDays
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	removed := 0
	err := s.store.UpdateTasks(func(tasks []ImageTask) []ImageTask {
		out := tasks[:0]
		for _, task := range tasks {
			if task.Status != "success" && task.Status != "partial_success" && task.Status != "error" && task.Status != "canceled" {
				out = append(out, task)
				continue
			}
			updated, err := parseTimeAny(task.UpdatedAt)
			if err != nil || updated.After(cutoff) {
				out = append(out, task)
				continue
			}
			removed++
		}
		return out
	})
	if err != nil {
		return fmt.Errorf("清理过期图片任务失败: %w", err)
	}
	if removed > 0 {
		if s.logSvc != nil {
			s.logSvc.add("system", "清理旧图片任务", map[string]any{"removed": removed, "retention_days": days})
		}
	}
	return nil
}

func (s *Server) updateTaskStatus(ownerID, id, status, errText string, data []map[string]any) (bool, error) {
	updated := false
	err := s.store.UpdateTasks(func(tasks []ImageTask) []ImageTask {
		for i := range tasks {
			if tasks[i].OwnerID == ownerID && tasks[i].ID == id && isActiveImageTaskStatus(tasks[i].Status) {
				tasks[i].Status = status
				tasks[i].Error = errText
				tasks[i].Data = data
				tasks[i].Terminal = !isActiveImageTaskStatus(status)
				tasks[i].StageCode, tasks[i].StageLabel = imageTaskStage(status)
				if status == "running" && tasks[i].StartedAt == "" {
					tasks[i].StartedAt = nowISO()
				}
				requested := max(1, tasks[i].RequestedCount)
				tasks[i].RequestedCount = requested
				tasks[i].SucceededCount = len(data)
				if tasks[i].SucceededCount > requested {
					tasks[i].SucceededCount = requested
				}
				if tasks[i].Terminal {
					tasks[i].PendingCount = 0
					tasks[i].FailedCount = max(0, requested-tasks[i].SucceededCount)
				} else {
					tasks[i].PendingCount = max(0, requested-tasks[i].SucceededCount)
					tasks[i].FailedCount = 0
				}
				if tasks[i].Terminal {
					tasks[i].DurationMS = time.Since(parseTaskCreatedAt(tasks[i].CreatedAt)).Milliseconds()
					tasks[i].ElapsedMS = tasks[i].DurationMS
				}
				tasks[i].UpdatedAt = nowISO()
				updated = true
				return tasks
			}
		}
		return tasks
	})
	return updated && err == nil, err
}

func imageTaskStage(status string) (string, string) {
	switch status {
	case "queued":
		return "queued", "排队中"
	case "running":
		return "running", "生成中"
	case "success":
		return "success", "成功"
	case "partial_success":
		return "partial_success", "部分成功"
	case "canceled":
		return "canceled", "已取消"
	case "error":
		return "error", "失败"
	default:
		return status, status
	}
}

func parseTaskCreatedAt(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Now()
}

func (s *Server) updateTaskProgress(ownerID, id, code, label string) (bool, error) {
	updated := false
	err := s.store.UpdateTasks(func(tasks []ImageTask) []ImageTask {
		for i := range tasks {
			if tasks[i].OwnerID != ownerID || tasks[i].ID != id || !isActiveImageTaskStatus(tasks[i].Status) {
				continue
			}
			tasks[i].StageCode, tasks[i].StageLabel = code, label
			tasks[i].UpdatedAt = nowISO()
			updated = true
			return tasks
		}
		return tasks
	})
	return updated && err == nil, err
}

func (s *Server) settleImageTask(task ImageTask, identity *Identity, charged int, status, errText string, data []map[string]any) (bool, error) {
	if identity != nil && identity.Role == "admin" {
		charged = 0
	}
	refund := 0
	if status != "success" {
		refund = charged
	}
	updated, err := s.store.settleImageTaskWithQuota(task.OwnerID, task.ID, status, errText, data, refund)
	if !updated && err == nil && identity != nil && identity.Role != "admin" && charged > 0 {
		for _, current := range s.store.LoadTasks() {
			if current.OwnerID == task.OwnerID && current.ID == task.ID && !current.QuotaSettled {
				s.refundImage(identity, charged)
				break
			}
		}
	}
	return updated, err
}

func retryImageTaskSettlement(settle func() (bool, error)) (bool, error) {
	var updated bool
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 100 * time.Millisecond)
		}
		updated, err = settle()
		if err == nil {
			return updated, nil
		}
	}
	return false, err
}

func (s *Server) imageTaskSettlementConfirmed(task ImageTask, status string, count int) bool {
	if status != "success" && status != "partial_success" {
		return false
	}
	for _, current := range s.store.LoadTasks() {
		if current.OwnerID == task.OwnerID && current.ID == task.ID {
			return current.Status == status && current.QuotaSettled && len(current.Data) == count
		}
	}
	return false
}

func (s *Server) chargeImageTask(identity *Identity, task ImageTask, n int) (bool, error) {
	if identity == nil {
		return false, errors.New("用户身份不存在")
	}
	return s.store.chargeImageTask(identity.ID, task.ID, n, identity.Role == "admin")
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) saveImageTaskResults(ctx context.Context, baseURL, prompt string, results []upstreamImageResult) ([]map[string]any, []string, error) {
	data := make([]map[string]any, 0, len(results))
	rels := make([]string, 0, len(results))
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return data, rels, err
		}
		rel, url, err := s.saveImageWithBaseURL(baseURL, result.Bytes)
		if err != nil {
			return data, rels, err
		}
		rels = append(rels, rel)
		data = append(data, map[string]any{
			"url":            url,
			"b64_json":       base64.StdEncoding.EncodeToString(result.Bytes),
			"revised_prompt": firstNonEmpty(result.RevisedPrompt, prompt),
		})
	}
	return data, rels, nil
}

func collectImageTaskGenerations(ctx context.Context, count int, generate func(context.Context) ([]upstreamImageResult, error)) ([]upstreamImageResult, error) {
	results := make([]upstreamImageResult, 0, count)
	var firstErr error
	for range count {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		items, err := generate(ctx)
		if err == nil && len(items) == 0 {
			err = errNoImageGenerated
		}
		if err != nil {
			if ctx.Err() != nil {
				return results, ctx.Err()
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results = append(results, items[0])
	}
	return results, firstErr
}

func isActiveImageTaskStatus(status string) bool {
	return status == "running" || status == "queued" || status == "pending"
}
func (s *Server) handleImageTaskGeneration(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b struct {
		ClientTaskID string `json:"client_task_id"`
		Prompt       string `json:"prompt"`
		Model        string `json:"model"`
		N            int    `json:"n"`
		Size         string `json:"size"`
		Resolution   string `json:"resolution"`
	}
	if !readBody(w, r, &b) {
		return
	}
	if b.N < 1 {
		b.N = 1
	}
	if b.N > 4 {
		b.N = 4
	}
	t := ImageTask{ID: firstNonEmpty(strings.TrimSpace(b.ClientTaskID), randID(8)), OwnerID: id.ID, Status: "queued", Terminal: false, Mode: "generate", Model: b.Model, Size: b.Size, Resolution: b.Resolution, RequestedCount: b.N, PendingCount: b.N, StageCode: "queued", StageLabel: "排队中", CreatedAt: nowISO(), UpdatedAt: nowISO()}
	existing, created, runner, err := s.admitImageTask(t)
	if err != nil {
		if errors.Is(err, errImageTaskQueueFull) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "创建图片任务失败")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, projectImageTask(existing))
		return
	}
	reserved := true
	defer func() {
		if reserved {
			runner.release()
		}
	}()
	callID := s.logCallStart(id, "/api/image-tasks/generations", b.Model, "文生图任务", b.Prompt)
	if err := s.checkContent(b.Prompt); err != nil {
		t.Status = "error"
		t.Error = err.Error()
		s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", err, nil)
		if _, updateErr := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); updateErr != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, 200, t)
		return
	}
	charged, chargeErr := s.chargeImageTask(id, t, b.N)
	if chargeErr != nil {
		t.Status, t.Error = "error", "扣除画图额度失败："+chargeErr.Error()
		if _, err := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, http.StatusInternalServerError, t)
		return
	}
	if !charged {
		t.Status = "error"
		t.Error = "画图额度不足"
		if _, err := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, 200, t)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.setTaskCancel(id.ID, t.ID, cancel)
	if !s.taskIsActive(id.ID, t.ID) {
		if stop := s.popTaskCancel(id.ID, t.ID); stop != nil {
			stop()
		}
		_, _ = s.settleImageTask(t, id, b.N, "canceled", "canceled", nil)
		t.Status, t.Error = "canceled", "canceled"
		t = projectImageTask(t)
		writeJSON(w, http.StatusOK, t)
		return
	}
	var settled atomic.Bool
	baseURL := s.baseURL(r)
	settle := func(status, errText string, data []map[string]any) (bool, error) {
		if !settled.CompareAndSwap(false, true) {
			return false, nil
		}
		charged := b.N
		if status == "partial_success" {
			charged = max(0, b.N-len(data))
		}
		updated, err := retryImageTaskSettlement(func() (bool, error) {
			return s.settleImageTask(t, id, charged, status, errText, data)
		})
		if err != nil {
			settled.Store(false)
		} else if !updated {
			updated = s.imageTaskSettlementConfirmed(t, status, len(data))
		}
		return updated, err
	}
	onDiscard := func(reason error) {
		cancel()
		s.popTaskCancel(t.OwnerID, t.ID)
		if settled.Load() {
			return
		}
		if _, err := settle("error", reason.Error(), nil); err != nil {
			reason = errors.Join(reason, err)
		}
		s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", reason, map[string]any{"task_id": t.ID})
	}
	job := imageTaskJob{key: imageTaskKey{t.OwnerID, t.ID}, onDiscard: onDiscard, run: func() {
		task, identity := t, id
		defer s.popTaskCancel(task.OwnerID, task.ID)
		defer cancel()
		if updated, err := s.updateTaskStatus(task.OwnerID, task.ID, "running", "", nil); err != nil || !updated {
			if err != nil {
				settle("error", err.Error(), nil)
			} else {
				settle("canceled", "canceled", nil)
			}
			return
		}
		_, _ = s.updateTaskProgress(task.OwnerID, task.ID, "generating", "上游生成中")
		runCtx, stop := context.WithTimeout(ctx, s.imageRequestTimeout())
		defer stop()
		generated, generationErr := collectImageTaskGenerations(runCtx, b.N, func(ctx context.Context) ([]upstreamImageResult, error) {
			return s.generateImageWithPool(ctx, b.Prompt, b.Model, b.Size, b.Resolution, nil)
		})
		if runCtx.Err() != nil {
			settle("canceled", "canceled", nil)
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", errors.New("canceled"), map[string]any{"task_id": task.ID})
			return
		}
		if len(generated) == 0 {
			if generationErr == nil {
				generationErr = errNoImageGenerated
			}
			settle("error", generationErr.Error(), nil)
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", generationErr, map[string]any{"task_id": task.ID})
			return
		}
		_, _ = s.updateTaskProgress(task.OwnerID, task.ID, "saving", "保存图片")
		data, rels, saveErr := s.saveImageTaskResults(runCtx, baseURL, b.Prompt, generated)
		if runCtx.Err() != nil {
			settle("canceled", "canceled", nil)
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", errors.New("canceled"), map[string]any{"task_id": task.ID})
			return
		}
		resultErr := errors.Join(generationErr, saveErr)
		if len(data) == 0 {
			if resultErr == nil {
				resultErr = errNoImageGenerated
			}
			settle("error", resultErr.Error(), nil)
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", resultErr, map[string]any{"task_id": task.ID})
			return
		}
		status, errText := "success", ""
		logFields := map[string]any{"task_id": task.ID, "image_count": len(data), "n": b.N}
		if len(data) < b.N {
			status = "partial_success"
			logFields["status"] = status
			logFields["failed_count"] = b.N - len(data)
			if resultErr != nil {
				errText = resultErr.Error()
				logFields["error"] = errText
			}
		}
		if updated, err := settle(status, errText, data); err != nil {
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", err, map[string]any{"task_id": task.ID, "image_count": len(data)})
		} else if updated {
			for _, rel := range rels {
				s.recordOwner(identity, rel)
				s.recordPrompt(rel, b.Prompt, false)
			}
			s.logCallSuccess(callID, "/api/image-tasks/generations", b.Model, "文生图任务", logFields)
		} else {
			s.logCallFailure(callID, "/api/image-tasks/generations", b.Model, "文生图任务", errors.New("task result discarded after cancellation"), map[string]any{"task_id": task.ID})
		}
	}}
	if !runner.submit(job) {
		onDiscard(errors.New("image task runner is closed"))
		writeErr(w, http.StatusServiceUnavailable, "图片任务队列已关闭")
		return
	}
	reserved = false
	if !s.taskIsActive(t.OwnerID, t.ID) {
		runner.cancelQueued(imageTaskKey{t.OwnerID, t.ID})
	}
	writeJSON(w, 200, t)
}
func (s *Server) handleImageTaskEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的图片编辑表单")
		return
	}
	prompt := r.FormValue("prompt")
	model := r.FormValue("model")
	if model == "" {
		model = "gpt-image-2"
	}
	t := ImageTask{ID: firstNonEmpty(strings.TrimSpace(r.FormValue("client_task_id")), randID(8)), OwnerID: id.ID, Status: "queued", Terminal: false, Mode: "edit", Model: model, Size: r.FormValue("size"), Resolution: r.FormValue("resolution"), RequestedCount: 1, PendingCount: 1, StageCode: "queued", StageLabel: "排队中", CreatedAt: nowISO(), UpdatedAt: nowISO()}
	existing, created, runner, err := s.admitImageTask(t)
	if err != nil {
		if errors.Is(err, errImageTaskQueueFull) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "创建图片任务失败")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, projectImageTask(existing))
		return
	}
	reserved := true
	defer func() {
		if reserved {
			runner.release()
		}
	}()
	callID := s.logCallStart(id, "/api/image-tasks/edits", model, "图生图任务", prompt)
	if err := s.checkContent(prompt); err != nil {
		t.Status = "error"
		t.Error = err.Error()
		s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", err, nil)
		if _, updateErr := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); updateErr != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, 200, t)
		return
	}
	charged, chargeErr := s.chargeImageTask(id, t, 1)
	if chargeErr != nil {
		t.Status, t.Error = "error", "扣除画图额度失败："+chargeErr.Error()
		if _, err := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, http.StatusInternalServerError, t)
		return
	}
	if !charged {
		t.Status = "error"
		t.Error = "画图额度不足"
		if _, err := s.updateTaskStatus(id.ID, t.ID, t.Status, t.Error, nil); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t = projectImageTask(t)
		writeJSON(w, 200, t)
		return
	}
	inputs := [][]byte{}
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(f)
			_ = f.Close()
			if len(b) > 0 {
				inputs = append(inputs, b)
			}
		}
	}
	if len(inputs) == 0 {
		if _, err := s.settleImageTask(t, id, 1, "error", "image file is required", nil); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新图片任务失败")
			return
		}
		t.Status, t.Error = "error", "image file is required"
		t = projectImageTask(t)
		writeJSON(w, 200, t)
		return
	}
	spoolDir, err := spoolImageTaskInputs(s.dataDir, inputs)
	if err != nil {
		s.settleImageTask(t, id, 1, "error", "保存编辑图片失败", nil)
		writeErr(w, http.StatusInternalServerError, "保存编辑图片失败")
		return
	}
	spoolOwned := true
	defer func() {
		if spoolOwned {
			os.RemoveAll(spoolDir)
		}
	}()
	inputCount := len(inputs)
	inputs = nil
	baseURL := s.baseURL(r)
	ctx, cancel := context.WithCancel(context.Background())
	s.setTaskCancel(id.ID, t.ID, cancel)
	if !s.taskIsActive(id.ID, t.ID) {
		if stop := s.popTaskCancel(id.ID, t.ID); stop != nil {
			stop()
		}
		_, _ = s.settleImageTask(t, id, 1, "canceled", "canceled", nil)
		t.Status, t.Error = "canceled", "canceled"
		t = projectImageTask(t)
		writeJSON(w, http.StatusOK, t)
		return
	}
	var settled atomic.Bool
	settle := func(status, errText string, data []map[string]any) (bool, error) {
		if !settled.CompareAndSwap(false, true) {
			return false, nil
		}
		return s.settleImageTask(t, id, 1, status, errText, data)
	}
	onDiscard := func(reason error) {
		cancel()
		s.popTaskCancel(t.OwnerID, t.ID)
		os.RemoveAll(spoolDir)
		if settled.Load() {
			return
		}
		if _, err := settle("error", reason.Error(), nil); err != nil {
			reason = errors.Join(reason, err)
		}
		s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", reason, map[string]any{"task_id": t.ID})
	}
	job := imageTaskJob{key: imageTaskKey{t.OwnerID, t.ID}, onDiscard: onDiscard, run: func() {
		task, identity := t, id
		defer s.popTaskCancel(task.OwnerID, task.ID)
		defer cancel()
		defer os.RemoveAll(spoolDir)
		if updated, err := s.updateTaskStatus(task.OwnerID, task.ID, "running", "", nil); err != nil || !updated {
			if err != nil {
				settle("error", err.Error(), nil)
			} else {
				settle("canceled", "canceled", nil)
			}
			return
		}
		_, _ = s.updateTaskProgress(task.OwnerID, task.ID, "generating", "上游生成中")
		inputs, err := loadImageTaskInputs(spoolDir, inputCount)
		if err != nil {
			settle("error", err.Error(), nil)
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", err, map[string]any{"task_id": task.ID})
			return
		}
		runCtx, stop := context.WithTimeout(ctx, s.imageRequestTimeout())
		defer stop()
		items, err := s.generateImageWithPool(runCtx, prompt, model, task.Size, task.Resolution, inputs)
		if err == nil && len(items) == 0 {
			err = errors.New("image generation returned no images")
		}
		if err != nil {
			if runCtx.Err() != nil {
				settle("canceled", "canceled", nil)
				s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", errors.New("canceled"), map[string]any{"task_id": task.ID})
			} else {
				settle("error", err.Error(), nil)
				s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", err, map[string]any{"task_id": task.ID})
			}
			return
		}
		if runCtx.Err() != nil {
			settle("canceled", "canceled", nil)
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", errors.New("canceled"), map[string]any{"task_id": task.ID})
			return
		}
		_, _ = s.updateTaskProgress(task.OwnerID, task.ID, "saving", "保存图片")
		res := items[0]
		rel, url, err := s.saveImageWithBaseURL(baseURL, res.Bytes)
		if err != nil {
			settle("error", err.Error(), nil)
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", err, map[string]any{"task_id": task.ID})
			return
		}
		if runCtx.Err() != nil {
			settle("canceled", "canceled", nil)
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", errors.New("canceled"), map[string]any{"task_id": task.ID})
			return
		}
		data := []map[string]any{{"url": url, "b64_json": base64.StdEncoding.EncodeToString(res.Bytes), "revised_prompt": firstNonEmpty(res.RevisedPrompt, prompt)}}
		if updated, err := settle("success", "", data); err != nil {
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", err, map[string]any{"task_id": task.ID})
		} else if updated {
			s.recordOwner(identity, rel)
			s.recordPrompt(rel, prompt, true)
			s.logCallSuccess(callID, "/api/image-tasks/edits", model, "图生图任务", map[string]any{"task_id": task.ID, "image_count": len(data)})
		} else {
			s.logCallFailure(callID, "/api/image-tasks/edits", model, "图生图任务", errors.New("task result discarded after cancellation"), map[string]any{"task_id": task.ID})
		}
	}}
	if !runner.submit(job) {
		onDiscard(errors.New("image task runner is closed"))
		writeErr(w, http.StatusServiceUnavailable, "图片任务队列已关闭")
		return
	}
	reserved = false
	spoolOwned = false
	if !s.taskIsActive(t.OwnerID, t.ID) {
		runner.cancelQueued(imageTaskKey{t.OwnerID, t.ID})
	}
	writeJSON(w, 200, t)
}
func (s *Server) handleImageTaskCancel(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	ids := []string{}
	if arr, ok := b["ids"].([]any); ok {
		for _, item := range arr {
			if v := strings.TrimSpace(strAny(item, "")); v != "" {
				ids = append(ids, v)
			}
		}
	}
	if v := strings.TrimSpace(strAny(b["id"], "")); v != "" {
		ids = append(ids, v)
	}
	canceled := []string{}
	canceledKeys := []imageTaskKey{}
	skipped := []string{}
	missing := []string{}
	err := s.store.UpdateTasks(func(tasks []ImageTask) []ImageTask {
		for _, id := range ids {
			idx := -1
			matches := 0
			for i := range tasks {
				if tasks[i].ID != id {
					continue
				}
				matches++
				if tasks[i].OwnerID == identity.ID {
					idx = i
					break
				}
				if identity.Role == "admin" {
					idx = i
				}
			}
			if idx < 0 && matches == 0 {
				missing = append(missing, id)
				continue
			}
			if idx < 0 || (identity.Role == "admin" && tasks[idx].OwnerID != identity.ID && matches > 1) {
				skipped = append(skipped, id)
				continue
			}
			if !isActiveImageTaskStatus(tasks[idx].Status) {
				skipped = append(skipped, id)
				continue
			}
			tasks[idx].Status = "canceled"
			tasks[idx].Error = "canceled"
			tasks[idx].Terminal = true
			tasks[idx].StageCode, tasks[idx].StageLabel = imageTaskStage("canceled")
			requested := max(1, tasks[idx].RequestedCount)
			if tasks[idx].Mode == "edit" {
				requested = 1
			}
			tasks[idx].RequestedCount = requested
			tasks[idx].SucceededCount = min(requested, max(0, len(tasks[idx].Data)))
			tasks[idx].PendingCount = 0
			tasks[idx].FailedCount = max(0, requested-tasks[idx].SucceededCount)
			tasks[idx].DurationMS = time.Since(parseTaskCreatedAt(tasks[idx].CreatedAt)).Milliseconds()
			tasks[idx].UpdatedAt = nowISO()
			canceled = append(canceled, id)
			canceledKeys = append(canceledKeys, imageTaskKey{tasks[idx].OwnerID, id})
		}
		return tasks
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "取消图片任务失败")
		return
	}
	for _, key := range canceledKeys {
		if s.imageTasks != nil && s.imageTasks.cancelQueued(key) {
			continue
		}
		if cancel := s.popTaskCancel(key.ownerID, key.id); cancel != nil {
			cancel()
		}
	}
	writeJSON(w, 200, map[string]any{"canceled": canceled, "skipped": skipped, "missing_ids": missing})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	model := strAny(b["model"], "auto")
	requestText := extractPrompt(b)
	callID := s.logCallStart(id, "/api/chat/stream", model, "聊天", requestText)
	if isImageChatRequest(b) {
		s.handleChatStreamImage(w, r, id, b, callID)
		return
	}
	if err := s.checkContent(requestText); err != nil {
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeChat(id, 1) {
		writeErr(w, 402, "对话额度不足")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.imageRequestTimeout())
	defer cancel()
	requestedUpstreamCID := strings.TrimSpace(strAny(b["upstream_conversation_id"], strAny(b["conversation_id"], "")))
	_ = requestedUpstreamCID
	upstreamCID := ""
	messages, err := s.messagesFromBodyWithFiles(b)
	if err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	events, errs := s.streamChatWithRetry(ctx, messages, model, "", "")
	w.Header().Set("Content-Type", "text/event-stream")
	text := ""
	cid := "conv_" + randID(8)
	upstreamMessageID := ""
	currentNode := ""
	accountToken := ""
	fileIDs := []string{}
	sedimentIDs := []string{}
	toolInvoked := any(nil)
	turnUseCase := ""
	blocked := false
	for ev := range events {
		if ev.ConversationID != "" {
			upstreamCID = ev.ConversationID
		}
		if ev.MessageID != "" {
			upstreamMessageID = ev.MessageID
		}
		if ev.CurrentNode != "" {
			currentNode = ev.CurrentNode
		}
		if ev.AccountToken != "" {
			accountToken = ev.AccountToken
		}
		fileIDs = unique(append(fileIDs, ev.FileIDs...))
		sedimentIDs = unique(append(sedimentIDs, ev.SedimentIDs...))
		if ev.ToolInvoked != nil {
			toolInvoked = ev.ToolInvoked
		}
		if ev.TurnUseCase != "" {
			turnUseCase = ev.TurnUseCase
		}
		if ev.Blocked {
			blocked = true
		}
		if ev.Delta == "" {
			continue
		}
		text += ev.Delta
		sse(w, map[string]any{"type": "conversation.delta", "delta": ev.Delta, "text": text, "conversation_id": firstNonEmpty(upstreamCID, cid), "upstream_conversation_id": upstreamCID, "message_id": upstreamMessageID, "current_node": currentNode, "file_ids": fileIDs, "sediment_ids": sedimentIDs, "blocked": ev.Blocked, "tool_invoked": ev.ToolInvoked, "turn_use_case": ev.TurnUseCase, "done": false})
	}
	if err := <-errs; err != nil {
		s.refundChat(id, 1)
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, map[string]any{"stream": true})
		sse(w, map[string]any{"type": "conversation.error", "error": err.Error(), "done": true})
		sseDone(w)
		return
	}
	doneCID := firstNonEmpty(upstreamCID, cid)
	if doneCID != "" && accountToken != "" && (len(fileIDs) > 0 || len(sedimentIDs) > 0 || toolInvoked == true) {
		imageText, ok := s.resolveChatStreamImages(ctx, r, id, accountToken, doneCID, fileIDs, sedimentIDs, text, len(extractChatImages(b)) > 0)
		if ok {
			text = imageText
			sse(w, map[string]any{"type": "conversation.delta", "delta": imageText, "text": imageText, "conversation_id": doneCID, "upstream_conversation_id": doneCID, "message_id": upstreamMessageID, "current_node": currentNode, "file_ids": fileIDs, "sediment_ids": sedimentIDs, "blocked": blocked, "tool_invoked": toolInvoked, "turn_use_case": turnUseCase, "done": false})
		}
	}
	s.upsertChatConversationFromStream(id, b, doneCID, upstreamMessageID, currentNode, "", text)
	s.logCallSuccess(callID, "/api/chat/stream", model, "聊天", map[string]any{"stream": true, "output_tokens": approxTokens(text), "file_ids": fileIDs, "sediment_ids": sedimentIDs})
	sse(w, map[string]any{"type": "conversation.done", "text": text, "conversation_id": doneCID, "upstream_conversation_id": doneCID, "message_id": upstreamMessageID, "current_node": currentNode, "file_ids": fileIDs, "sediment_ids": sedimentIDs, "blocked": blocked, "tool_invoked": toolInvoked, "turn_use_case": turnUseCase, "done": true})
	sseDone(w)
}
func (s *Server) resolveChatStreamImages(ctx context.Context, r *http.Request, id *Identity, token, conversationID string, fileIDs, sedimentIDs []string, fallbackText string, isEdit bool) (string, bool) {
	client, err := s.newUpstreamClientForAccount(s.accountByToken(token))
	if err != nil {
		return fallbackText, false
	}
	if len(fileIDs) == 0 && len(sedimentIDs) == 0 && conversationID != "" {
		opts := s.imageGenerationOptions()
		f, sed := client.pollImageIDs(ctx, conversationID, opts.Timeout, opts.PollInterval, opts.PollInitialWait)
		fileIDs = append(fileIDs, f...)
		sedimentIDs = append(sedimentIDs, sed...)
	}
	urls, err := client.resolveImageURLs(ctx, conversationID, fileIDs, sedimentIDs)
	if err != nil || len(urls) == 0 {
		return fallbackText, false
	}
	data := []map[string]any{}
	for _, u := range urls {
		bytes, err := client.download(ctx, u)
		if err != nil {
			continue
		}
		rel, url, err := s.saveImage(r, bytes)
		if err != nil {
			continue
		}
		s.recordOwner(id, rel)
		s.recordPrompt(rel, fallbackText, isEdit)
		data = append(data, map[string]any{"url": url, "b64_json": base64.StdEncoding.EncodeToString(bytes), "revised_prompt": fallbackText})
	}
	if len(data) == 0 {
		return fallbackText, false
	}
	return buildChatImageMarkdown(data), true
}

func (s *Server) handleChatStreamImage(w http.ResponseWriter, r *http.Request, id *Identity, b map[string]any, callID string) {
	model := strAny(b["model"], "gpt-image-2")
	if !isSupportedImageModel(model) {
		model = "gpt-image-2"
	}
	prompt := extractChatPrompt(b)
	if prompt == "" {
		err := errors.New("prompt is required")
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.checkContent(prompt); err != nil {
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, nil)
		writeErr(w, 400, err.Error())
		return
	}
	if !s.consumeImage(id, 1) {
		writeErr(w, 402, "画图额度不足")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.imageRequestTimeout())
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	items, err := streamImageChatWithKeepalive(ctx, w, 15*time.Second, func() ([]upstreamImageResult, error) {
		return s.generateImageWithPool(ctx, prompt, model, strAny(b["size"], ""), strAny(b["resolution"], ""), extractChatImages(b))
	})
	cid := "conv_" + randID(8)
	if err != nil {
		s.refundImage(id, 1)
		s.logCallFailure(callID, "/api/chat/stream", model, "聊天", err, map[string]any{"image": true})
		sse(w, map[string]any{"type": "conversation.error", "error": err.Error(), "done": true})
		sseDone(w)
		return
	}
	data := []map[string]any{}
	for _, res := range items {
		rel, url, err := s.saveImage(r, res.Bytes)
		if err != nil {
			s.refundImage(id, 1)
			sse(w, map[string]any{"type": "conversation.error", "error": err.Error(), "done": true})
			sseDone(w)
			return
		}
		s.recordOwner(id, rel)
		s.recordPrompt(rel, prompt, len(extractChatImages(b)) > 0)
		data = append(data, map[string]any{"url": url, "b64_json": base64.StdEncoding.EncodeToString(res.Bytes), "revised_prompt": firstNonEmpty(res.RevisedPrompt, prompt)})
		break
	}
	text := buildChatImageMarkdown(data)
	s.logCallSuccess(callID, "/api/chat/stream", model, "聊天", map[string]any{"image": true, "image_count": len(data)})
	sse(w, map[string]any{"type": "conversation.delta", "delta": text, "text": text, "conversation_id": cid, "done": false})
	sse(w, map[string]any{"type": "conversation.done", "text": text, "conversation_id": cid, "done": true})
	sseDone(w)
}

func streamImageChatWithKeepalive(ctx context.Context, w http.ResponseWriter, interval time.Duration, generate func() ([]upstreamImageResult, error)) ([]upstreamImageResult, error) {
	type result struct {
		items []upstreamImageResult
		err   error
	}
	results := make(chan result, 1)
	fmt.Fprint(w, ": stream-open\n\n")
	flushSSE(w)
	go func() {
		items, err := generate()
		results <- result{items: items, err: err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case completed := <-results:
			return completed.items, completed.err
		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flushSSE(w)
		}
	}
}

func (s *Server) upsertChatConversationFromStream(id *Identity, b map[string]any, upstreamCID, messageID, currentNode, token, assistantText string) {
	convID := strings.TrimSpace(strAny(b["id"], strAny(b["conversation_local_id"], "")))
	if convID == "" {
		return
	}
	item := map[string]any{}
	for k, v := range b {
		item[k] = v
	}
	item["id"] = convID
	item["owner_id"] = id.ID
	item["upstream_conversation_id"] = upstreamCID
	item["upstream_message_id"] = messageID
	item["current_node"] = currentNode
	item["upstream_account_token"] = token
	item["updated_at"] = nowISO()
	if item["created_at"] == nil || strings.TrimSpace(strAny(item["created_at"], "")) == "" {
		item["created_at"] = nowISO()
	}
	if strings.TrimSpace(strAny(item["title"], "")) == "" {
		item["title"] = truncateText(extractPrompt(b), 40)
	}
	if assistantText != "" {
		item["last_text"] = truncateText(assistantText, 500)
	}
	_ = s.store.UpdateList("chat_conversations.json", func(items []map[string]any) []map[string]any {
		out := []map[string]any{item}
		for _, it := range items {
			if strAny(it["id"], "") != convID {
				out = append(out, it)
			}
		}
		return out
	})
}

func (s *Server) handleChatAccountTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIdentity(w, r); !ok {
		return
	}
	types := map[string]bool{}
	for _, a := range s.store.LoadAccounts() {
		types[a.Type] = true
	}
	arr := []map[string]any{}
	keys := make([]string, 0, len(types))
	for t := range types {
		if t != "" {
			keys = append(keys, t)
		}
	}
	sort.Strings(keys)
	for _, t := range keys {
		arr = append(arr, map[string]any{"type": t, "label": t})
	}
	if len(arr) == 0 {
		arr = []map[string]any{{"type": "free", "label": "free"}}
	}
	writeJSON(w, 200, map[string]any{"items": arr})
}
func (s *Server) handleChatConversations(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		items := []map[string]any{}
		for _, it := range s.store.LoadList("chat_conversations.json") {
			if strAny(it["owner_id"], identity.ID) == identity.ID || identity.Role == "admin" {
				items = append(items, it)
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if r.Method == http.MethodPost {
		var b map[string]any
		if !readBody(w, r, &b) {
			return
		}
		if strings.TrimSpace(strAny(b["id"], "")) == "" {
			b["id"] = "conv_" + randID(8)
		}
		b["owner_id"] = identity.ID
		b["updated_at"] = nowISO()
		if strings.TrimSpace(strAny(b["created_at"], "")) == "" {
			b["created_at"] = nowISO()
		}
		conflict := false
		_ = s.store.UpdateList("chat_conversations.json", func(items []map[string]any) []map[string]any {
			out := []map[string]any{b}
			for _, it := range items {
				if strAny(it["id"], "") == strAny(b["id"], "") {
					owner := strAny(it["owner_id"], "")
					if owner != "" && owner != identity.ID && identity.Role != "admin" {
						conflict = true
						return items
					}
					continue
				}
				out = append(out, it)
			}
			return out
		})
		if conflict {
			writeErr(w, 403, "不能覆盖其他用户的会话")
			return
		}
		writeJSON(w, 200, map[string]any{"item": b})
		return
	}
	writeErr(w, 405, "method not allowed")
}
func (s *Server) handleChatConversationID(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/chat/conversations/")
	items := s.store.LoadList("chat_conversations.json")
	out := []map[string]any{}
	deleted := false
	for _, it := range items {
		owner := strAny(it["owner_id"], "")
		if strAny(it["id"], "") == id && (owner == "" || owner == identity.ID || identity.Role == "admin") {
			deleted = true
			upstreamCID := strings.TrimSpace(strAny(it["upstream_conversation_id"], ""))
			upstreamToken := strings.TrimSpace(strAny(it["upstream_account_token"], ""))
			if upstreamCID != "" && upstreamToken != "" {
				go func(cid, token string) {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if c, err := s.newUpstreamClientForAccount(s.accountByToken(token)); err == nil {
						c.DeleteConversation(ctx, cid)
					}
				}(upstreamCID, upstreamToken)
			}
			continue
		}
		out = append(out, it)
	}
	_ = s.store.SaveList("chat_conversations.json", out)
	writeJSON(w, 200, map[string]any{"ok": deleted})
}

func (s *Server) publicGalleryItem(r *http.Request, it GalleryItem) map[string]any {
	return map[string]any{"id": it.ID, "image_rel": it.ImageRel, "image_url": s.baseURL(r) + "/images/" + it.ImageRel, "url": s.baseURL(r) + "/images/" + it.ImageRel, "publisher_name": it.PublisherName, "prompt": it.Prompt, "model": it.Model, "size": it.Size, "width": it.Width, "height": it.Height, "is_edit": it.IsEdit, "created_at": it.CreatedAt, "status": it.Status}
}
func (s *Server) handleGalleryFeed(w http.ResponseWriter, r *http.Request) {
	items := s.store.LoadGallery()
	out := []map[string]any{}
	for _, it := range items {
		if it.Status == "" || it.Status == "visible" {
			out = append(out, s.publicGalleryItem(r, it))
		}
	}
	writeJSON(w, 200, map[string]any{"items": out, "total": len(out)})
}
func (s *Server) handleGalleryPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	var b map[string]any
	if !readBody(w, r, &b) {
		return
	}
	rel := relClean(strAny(b["image_rel"], strAny(b["path"], strAny(b["url"], ""))))
	rel = relFromURL(rel)
	var err error
	rel, err = safeImageRel(rel)
	if err != nil {
		writeErr(w, 400, "invalid image_rel")
		return
	}
	it := GalleryItem{ID: randID(8), ImageRel: rel, PublisherID: id.ID, PublisherName: id.Name, Prompt: strAny(b["prompt"], ""), Model: strAny(b["model"], "gpt-image-2"), Size: strAny(b["size"], ""), CreatedAt: time.Now().Unix(), Status: "visible"}
	items := s.store.LoadGallery()
	items = append([]GalleryItem{it}, items...)
	_ = s.store.SaveGallery(items)
	writeJSON(w, 200, map[string]any{"item": s.publicGalleryItem(r, it)})
}
func (s *Server) handleGalleryPublishedBatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIdentity(w, r); !ok {
		return
	}
	var b struct {
		Paths     []string `json:"paths"`
		ImageRels []string `json:"image_rels"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	set := map[string]bool{}
	for _, it := range s.store.LoadGallery() {
		set[it.ImageRel] = true
	}
	res := map[string]bool{}
	for _, p := range append(b.Paths, b.ImageRels...) {
		rel, err := safeImageRel(p)
		if err != nil {
			continue
		}
		res[rel] = set[rel]
	}
	writeJSON(w, 200, map[string]any{"items": res})
}
func (s *Server) handleGalleryPublished(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireIdentity(w, r); !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"items": s.store.LoadGallery()})
}
func (s *Server) handleGalleryItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/gallery/items/")
	action := ""
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		id = parts[0]
		action = parts[1]
	}
	items := s.store.LoadGallery()
	idx := -1
	for i, it := range items {
		if it.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, 404, "not found")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"item": s.publicGalleryItem(r, items[idx])})
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if identity.Role != "admin" && items[idx].PublisherID != identity.ID {
		writeErr(w, 403, "只能修改自己发布的 gallery item")
		return
	}
	if r.Method == http.MethodDelete {
		items = append(items[:idx], items[idx+1:]...)
		_ = s.store.SaveGallery(items)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method == http.MethodPost && action == "hide" {
		items[idx].Status = "hidden"
		_ = s.store.SaveGallery(items)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method == http.MethodPost && action == "unhide" {
		items[idx].Status = "visible"
		_ = s.store.SaveGallery(items)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeErr(w, 405, "method not allowed")
}

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		path := filepath.Join(s.webDist, filepath.Clean(r.URL.Path))
		if st, err := os.Stat(path); err == nil {
			if st.IsDir() {
				path = filepath.Join(path, "index.html")
				st, err = os.Stat(path)
			}
			if err == nil && !st.IsDir() {
				http.ServeFile(w, r, path)
				return
			}
		}
	}
	index := filepath.Join(s.webDist, "index.html")
	if _, err := os.Stat(index); err == nil {
		http.ServeFile(w, r, index)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "service": "chatgpt2api-go", "hint": "web_dist/index.html not found; build frontend to enable SPA"})
}
