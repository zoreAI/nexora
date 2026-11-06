package app

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

type sentinelIdentity struct {
	UserAgent           string `json:"user_agent"`
	Timezone            string `json:"timezone"`
	DateString          string `json:"date_string"`
	ScreenSize          int    `json:"screen_size"`
	HeapLimit           int64  `json:"heap_limit"`
	Language            string `json:"language"`
	Languages           string `json:"languages"`
	HardwareConcurrency int    `json:"hardware_concurrency"`
	Vendor              string `json:"vendor"`
}

type sentinelLocalSolver interface {
	Identity(context.Context) (sentinelIdentity, error)
	Turnstile(context.Context, map[string]any, string, string) (string, error)
}

//go:embed sentinel_vm.js
var sentinelVMScript string

type browserSentinelSolver struct {
	mu          sync.Mutex
	proxy       string
	userAgent   string
	pageContext context.Context
	cancelPage  context.CancelFunc
	cancelAlloc context.CancelFunc
	profileDir  string
	openedAt    time.Time
}

var sentinelSolvers = struct {
	sync.Mutex
	items map[string]*browserSentinelSolver
}{items: make(map[string]*browserSentinelSolver)}

func sharedSentinelSolver(proxy, userAgent string) sentinelLocalSolver {
	key := proxy + "\x00" + userAgent
	sentinelSolvers.Lock()
	defer sentinelSolvers.Unlock()
	if solver := sentinelSolvers.items[key]; solver != nil {
		return solver
	}
	solver := &browserSentinelSolver{proxy: proxy, userAgent: userAgent}
	sentinelSolvers.items[key] = solver
	return solver
}

func CloseSentinelSolvers() {
	sentinelSolvers.Lock()
	items := sentinelSolvers.items
	sentinelSolvers.items = make(map[string]*browserSentinelSolver)
	sentinelSolvers.Unlock()
	for _, solver := range items {
		solver.mu.Lock()
		solver.close()
		solver.mu.Unlock()
	}
}

func (s *browserSentinelSolver) close() {
	if s.cancelPage != nil {
		s.cancelPage()
	}
	if s.cancelAlloc != nil {
		s.cancelAlloc()
	}
	if s.profileDir != "" {
		_ = os.RemoveAll(s.profileDir)
	}
	s.pageContext = nil
	s.cancelPage = nil
	s.cancelAlloc = nil
	s.profileDir = ""
	s.openedAt = time.Time{}
}

func findSentinelChrome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CHATGPT2API_CHROME_BIN")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("CHATGPT2API_CHROME_BIN %q: %w", configured, err)
		}
		return path, nil
	}
	for _, candidate := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"/snap/bin/chromium",
	} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Chrome/Chromium not found; install it or set CHATGPT2API_CHROME_BIN")
}

func (s *browserSentinelSolver) runContext(request context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(s.pageContext, timeout)
	stop := context.AfterFunc(request, cancel)
	return ctx, func() { stop(); cancel() }
}

func (s *browserSentinelSolver) ensurePage(request context.Context) error {
	if s.pageContext != nil && time.Since(s.openedAt) < 30*time.Minute {
		ctx, cancel := s.runContext(request, 5*time.Second)
		_, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`location.hostname === 'chatgpt.com'`))
		cancel()
		if err == nil {
			return nil
		}
	}
	s.close()
	chromePath, err := findSentinelChrome()
	if err != nil {
		return err
	}
	if strings.Contains(s.proxy, "@") {
		return errors.New("authenticated proxy is not supported by local Chrome; use an unauthenticated local proxy")
	}
	profileDir, err := os.MkdirTemp("", "nexora-chrome-")
	if err != nil {
		return fmt.Errorf("create Chrome profile: %w", err)
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.ExecPath(chromePath),
		remote.WebSocket,
		chromedp.Flag("headless", true),
		chromedp.Flag("remote-debugging-port", "0"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-software-rasterizer", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("user-data-dir", filepath.Clean(profileDir)),
		chromedp.WindowSize(1280, 800),
	)
	if s.proxy != "" {
		opts = append(opts, chromedp.ProxyServer(s.proxy))
	}
	if os.Geteuid() == 0 {
		opts = append(opts, chromedp.Flag("no-sandbox", true))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	pageCtx, cancelPage := chromedp.NewContext(allocCtx)
	s.pageContext, s.cancelPage, s.cancelAlloc, s.profileDir = pageCtx, cancelPage, cancelAlloc, profileDir
	// The first Run owns the browser lifetime, so bound startup by canceling the
	// page itself and stop that timer once Chrome has launched.
	startupTimer := time.AfterFunc(60*time.Second, cancelPage)
	stopRequest := context.AfterFunc(request, cancelPage)
	err = chromedp.Do(pageCtx)
	startupTimer.Stop()
	stopRequest()
	if err != nil {
		s.close()
		return fmt.Errorf("start Chrome executable %q: %w (set CHATGPT2API_CHROME_BIN to a runnable Chromium binary or use the direct HTTP recovery path)", chromePath, err)
	}
	ctx, cancel := s.runContext(request, 60*time.Second)
	defer cancel()
	target := chromedp.FromContext(pageCtx).Target
	major := "146"
	if match := regexp.MustCompile(`Chrome/(\d+)`).FindStringSubmatch(s.userAgent); len(match) > 1 {
		major = match[1]
	}
	version := major + ".0.0.0"
	brands := []*emulation.UserAgentBrandVersion{
		{Brand: "Chromium", Version: major},
		{Brand: "Google Chrome", Version: major},
		{Brand: "Not A(Brand", Version: "99"},
	}
	fullBrands := []*emulation.UserAgentBrandVersion{
		{Brand: "Chromium", Version: version},
		{Brand: "Google Chrome", Version: version},
		{Brand: "Not A(Brand", Version: "99.0.0.0"},
	}
	_, err = cdp.Call(ctx, target, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{
		UserAgent: s.userAgent, AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8", Platform: "Win32",
		UserAgentMetadata: &emulation.UserAgentMetadata{
			Brands: brands, FullVersionList: fullBrands, Platform: "Windows", PlatformVersion: "19.0.0",
			Architecture: "x86", Model: "", Mobile: false, Bitness: "64", Wow64: false,
		},
	})
	if err == nil {
		_, err = cdp.Call(ctx, target, emulation.SetLocaleOverride, emulation.SetLocaleOverrideParams{Locale: "en_US"})
	}
	if err == nil {
		_, err = cdp.Call(ctx, target, emulation.SetTimezoneOverride, emulation.SetTimezoneOverrideParams{TimezoneID: localTimezoneName()})
	}
	if err == nil {
		_, err = cdp.Call(ctx, target, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{
			Source: `Object.defineProperty(navigator, 'webdriver', {get: () => undefined}); window.chrome = window.chrome || {runtime: {}};`,
		})
	}
	if err == nil {
		_, err = cdp.Call(ctx, target, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: sentinelVMScript})
	}
	if err == nil {
		err = chromedp.Do(ctx, chromedp.Navigate(chatGPTBaseURL+"/"))
	}
	if err != nil {
		s.close()
		return fmt.Errorf("open Chrome page: %w", err)
	}
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		ctx, cancel := s.runContext(request, 10*time.Second)
		ready, checkErr := chromedp.Run(ctx, chromedp.Evaluate[bool](`location.hostname === 'chatgpt.com' && document.readyState !== 'loading' && typeof window.__sentinelVM === 'object' && !/Just a moment|请稍候|Attention Required/i.test(document.title)`))
		cancel()
		if checkErr == nil && ready {
			s.openedAt = time.Now()
			return nil
		}
		select {
		case <-request.Done():
			s.close()
			return request.Err()
		case <-deadline.C:
			s.close()
			return errors.New("chatgpt.com page did not clear the browser challenge")
		case <-time.After(time.Second):
		}
	}
}

func (s *browserSentinelSolver) Identity(ctx context.Context) (sentinelIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensurePage(ctx); err != nil {
		return sentinelIdentity{}, err
	}
	runCtx, cancel := s.runContext(ctx, 10*time.Second)
	defer cancel()
	identity, err := chromedp.Run(runCtx, chromedp.Evaluate[sentinelIdentity](`({user_agent:navigator.userAgent,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone,date_string:String(new Date()),screen_size:screen.width+screen.height,heap_limit:performance.memory?.jsHeapSizeLimit||0,language:navigator.language,languages:navigator.languages.join(','),hardware_concurrency:navigator.hardwareConcurrency,vendor:navigator.vendor})`))
	if err != nil {
		return sentinelIdentity{}, err
	}
	if identity.UserAgent == "" || identity.Timezone == "" || identity.DateString == "" {
		return sentinelIdentity{}, errors.New("browser identity is incomplete")
	}
	return identity, nil
}

func (s *browserSentinelSolver) Turnstile(ctx context.Context, requirements map[string]any, p, dx string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensurePage(ctx); err != nil {
		return "", err
	}
	requirementsJSON, err := json.Marshal(requirements)
	if err != nil {
		return "", err
	}
	pJSON, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	script := strings.ReplaceAll(sentinelSDKScript, "__SENTINEL_REQUIREMENTS__", string(requirementsJSON))
	script = strings.ReplaceAll(script, "__SENTINEL_P__", string(pJSON))
	var last string
	if len(requirements) > 0 {
		runCtx, cancel := s.runContext(ctx, 90*time.Second)
		token, runErr := chromedp.Run(runCtx, chromedp.Evaluate[string](script, chromedp.EvalAwaitPromise))
		cancel()
		if runErr == nil && invalidSentinelToken(token, p) == "" {
			return token, nil
		}
		if runErr != nil {
			last = runErr.Error()
		} else {
			last = invalidSentinelToken(token, p)
		}
	}
	if dx == "" {
		return "", fmt.Errorf("SDK solve failed and no VM challenge was supplied: %s", last)
	}
	dxJSON, err := json.Marshal(dx)
	if err != nil {
		return "", err
	}
	vmScript := `(async () => String(await window.__sentinelVM.turnstile(` + string(dxJSON) + `, ` + string(pJSON) + `)))()`
	for attempt := 0; attempt < 3; attempt++ {
		runCtx, cancel := s.runContext(ctx, 15*time.Second)
		token, runErr := chromedp.Run(runCtx, chromedp.Evaluate[string](vmScript, chromedp.EvalAwaitPromise))
		cancel()
		if runErr != nil {
			return "", fmt.Errorf("VM solve after SDK failure (%s): %w", last, runErr)
		}
		if reason := invalidSentinelToken(token, p); reason == "" {
			return token, nil
		} else {
			last = reason
		}
		if last != "solver returned a VM step count" {
			break
		}
	}
	return "", fmt.Errorf("SDK and VM solve failed: %s", last)
}

func invalidSentinelToken(token, p string) string {
	if token == "" {
		return "empty token"
	}
	if strings.HasPrefix(token, "ERR:") {
		return strings.TrimPrefix(token, "ERR:")
	}
	if token == p {
		return "solver echoed the requirements token"
	}
	if regexp.MustCompile(`^\d+$`).MatchString(token) {
		return "solver returned a VM step count"
	}
	if decoded, err := base64.StdEncoding.DecodeString(token); err == nil && regexp.MustCompile(`^\d+:\s+`).Match(decoded) {
		return "solver returned a VM error"
	}
	return ""
}

const sentinelSDKScript = `(async () => {
  const requirements = __SENTINEL_REQUIREMENTS__;
  const key = __SENTINEL_P__;
  const candidates = [];
  const add = (url) => {
    const match = String(url || '').match(/\/assets\/(turnstile-[A-Za-z0-9_-]+\.js)/);
    if (match) candidates.push('/unauth-mweb/assets/' + match[1]);
  };
  add(window.__chatgpt2apiTurnstileAsset);
  try { performance.getEntriesByType('resource').forEach(entry => add(entry.name)); } catch {}
  Array.from(document.scripts || []).forEach(script => add(script.src));
  if (!candidates.length) {
    try {
      const html = await (await fetch('/', {credentials:'include'})).text();
      const scripts = [...html.matchAll(/([A-Za-z0-9._-]+\.js)/g)].slice(0, 12);
      for (const item of scripts) {
        try {
          const text = await (await fetch('/unauth-mweb/assets/' + item[1], {credentials:'include', signal:AbortSignal.timeout(4000)})).text();
          const found = text.match(/turnstile-[A-Za-z0-9_-]+\.js/);
          if (found) { add('/unauth-mweb/assets/' + found[0]); break; }
        } catch {}
      }
    } catch {}
  }
  candidates.push('/unauth-mweb/assets/turnstile-DykPLDEM.js');
  for (const asset of [...new Set(candidates)]) {
    try {
      const mod = await import(asset);
      const turnstile = mod.turnstile || mod.default?.turnstile;
      if (typeof turnstile?.getEnforcementToken !== 'function') continue;
      window.__chatgpt2apiTurnstileAsset = asset;
      const original = WeakMap.prototype.get;
      const cache = turnstile.tokenCache;
      WeakMap.prototype.get = function(target) {
        if (cache && this === cache) return original.call(this, target);
        return target === requirements ? key : original.call(this, target);
      };
      try {
        const token = String(await turnstile.getEnforcementToken(requirements) ?? '');
        if (!token || token === key || /^\d+$/.test(token)) return 'ERR:invalid SDK token';
        try { if (/^\d+:\s+.+/s.test(atob(token))) return 'ERR:SDK VM error'; } catch {}
        return token;
      } finally { WeakMap.prototype.get = original; }
    } catch {}
  }
  return 'ERR:cannot load turnstile module in browser page';
})()`
