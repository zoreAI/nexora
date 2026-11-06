package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	maxImageReferenceBytes = 50 << 20
	maxImageInputBytes     = 100 << 20
	maxImageInputs         = 16
	maxImageRedirects      = 3
	maxImageJSONBytes      = 140 << 20
)

type imageEditRequest struct {
	Prompt         string
	Model          string
	N              int
	Size           string
	Resolution     string
	ResponseFormat string
	Inputs         [][]byte
	Markup         []byte
}

var imageFetchSlots = make(chan struct{}, 8)

func parseImageEditRequest(r *http.Request) (imageEditRequest, error) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType == "application/json" {
		return parseJSONImageEditRequest(r)
	}
	if contentType != "" && contentType != "multipart/form-data" {
		return imageEditRequest{}, errors.New("content-type must be multipart/form-data or application/json")
	}
	return parseMultipartImageEditRequest(r)
}

func parseJSONImageEditRequest(r *http.Request) (imageEditRequest, error) {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxImageJSONBytes+1))
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return imageEditRequest{}, errors.New("invalid JSON body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return imageEditRequest{}, errors.New("invalid JSON body")
	}
	return parseJSONImageEditBody(r.Context(), body)
}

func parseJSONImageEditBody(ctx context.Context, body map[string]any) (imageEditRequest, error) {
	if body == nil {
		return imageEditRequest{}, errors.New("JSON body must be an object")
	}
	prompt := strings.TrimSpace(strAny(body["prompt"], ""))
	if prompt == "" {
		return imageEditRequest{}, errors.New("prompt is required")
	}
	n, err := parseImageCount(body["n"])
	if err != nil {
		return imageEditRequest{}, err
	}
	request := imageEditRequest{
		Prompt:         prompt,
		Model:          firstNonEmpty(strings.TrimSpace(strAny(body["model"], "")), "gpt-image-2"),
		N:              n,
		Size:           strings.TrimSpace(strAny(body["size"], "")),
		Resolution:     strings.TrimSpace(strAny(body["resolution"], "")),
		ResponseFormat: strings.TrimSpace(strAny(body["response_format"], "")),
	}
	if _, hasMask := body["mask"]; hasMask {
		if _, hasMarkup := body["markup"]; hasMarkup {
			return imageEditRequest{}, errors.New("markup and mask cannot be combined")
		}
		return imageEditRequest{}, errors.New("mask is not supported by the ChatGPT Web image route")
	}
	var sources []any
	for _, key := range []string{"images", "image", "image_url"} {
		if value, ok := body[key]; ok {
			sources = append(sources, value)
		}
	}
	if len(sources) == 0 {
		return imageEditRequest{}, errors.New("image file or image_url is required")
	}
	remaining := maxImageInputs
	totalBytes := 0
	for _, source := range sources {
		inputs, err := readImageSource(ctx, source, &remaining, &totalBytes)
		if err != nil {
			return imageEditRequest{}, err
		}
		request.Inputs = append(request.Inputs, inputs...)
		if len(request.Inputs) > 0 {
			if err := validateImageInputSet(request.Inputs); err != nil {
				return imageEditRequest{}, err
			}
		}
	}
	if err := validateImageInputSet(request.Inputs); err != nil {
		return imageEditRequest{}, err
	}
	if markup, ok := body["markup"]; ok {
		request.Markup, err = readMarkupSource(ctx, markup)
		if err != nil {
			return imageEditRequest{}, err
		}
	}
	return request, nil
}

func parseMultipartImageEditRequest(r *http.Request) (imageEditRequest, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxImageInputBytes+8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return imageEditRequest{}, errors.New("invalid multipart form")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		return imageEditRequest{}, errors.New("prompt is required")
	}
	n, err := parseImageCount(r.FormValue("n"))
	if err != nil {
		return imageEditRequest{}, err
	}
	request := imageEditRequest{
		Prompt:         prompt,
		Model:          firstNonEmpty(strings.TrimSpace(r.FormValue("model")), "gpt-image-2"),
		N:              n,
		Size:           strings.TrimSpace(r.FormValue("size")),
		Resolution:     strings.TrimSpace(r.FormValue("resolution")),
		ResponseFormat: strings.TrimSpace(r.FormValue("response_format")),
	}
	if r.MultipartForm == nil {
		return imageEditRequest{}, errors.New("image file is required")
	}
	if _, hasMaskFile := r.MultipartForm.File["mask"]; hasMaskFile {
		if _, hasMarkupFile := r.MultipartForm.File["markup"]; hasMarkupFile {
			return imageEditRequest{}, errors.New("markup and mask cannot be combined")
		}
		return imageEditRequest{}, errors.New("mask is not supported by the ChatGPT Web image route")
	}
	if _, hasMaskField := r.MultipartForm.Value["mask"]; hasMaskField {
		if _, hasMarkupFile := r.MultipartForm.File["markup"]; hasMarkupFile {
			return imageEditRequest{}, errors.New("markup and mask cannot be combined")
		}
		if _, hasMarkupField := r.MultipartForm.Value["markup"]; hasMarkupField {
			return imageEditRequest{}, errors.New("markup and mask cannot be combined")
		}
		return imageEditRequest{}, errors.New("mask is not supported by the ChatGPT Web image route")
	}
	for _, key := range []string{"image", "image[]"} {
		for _, header := range r.MultipartForm.File[key] {
			file, err := header.Open()
			if err != nil {
				return imageEditRequest{}, fmt.Errorf("open image file: %w", err)
			}
			data, readErr := readLimitedImage(file)
			closeErr := file.Close()
			if readErr != nil {
				return imageEditRequest{}, readErr
			}
			if closeErr != nil {
				return imageEditRequest{}, fmt.Errorf("close image file: %w", closeErr)
			}
			request.Inputs = append(request.Inputs, data)
		}
	}
	if err := validateImageInputSet(request.Inputs); err != nil {
		return imageEditRequest{}, err
	}
	if files := r.MultipartForm.File["markup"]; len(files) > 0 {
		if len(files) != 1 || len(r.MultipartForm.Value["markup"]) > 0 {
			return imageEditRequest{}, errors.New("exactly one markup image is allowed")
		}
		file, err := files[0].Open()
		if err != nil {
			return imageEditRequest{}, fmt.Errorf("open markup file: %w", err)
		}
		request.Markup, err = readLimitedImage(file)
		closeErr := file.Close()
		if err != nil {
			return imageEditRequest{}, err
		}
		if closeErr != nil {
			return imageEditRequest{}, fmt.Errorf("close markup file: %w", closeErr)
		}
	} else if values := r.MultipartForm.Value["markup"]; len(values) > 0 {
		if len(values) != 1 {
			return imageEditRequest{}, errors.New("exactly one markup image is allowed")
		}
		request.Markup, err = readMarkupSource(r.Context(), values[0])
		if err != nil {
			return imageEditRequest{}, err
		}
	}
	return request, nil
}

func readMarkupSource(ctx context.Context, source any) ([]byte, error) {
	remaining, total := 1, 0
	images, err := readImageSource(ctx, source, &remaining, &total)
	if err != nil {
		return nil, fmt.Errorf("markup: %w", err)
	}
	if len(images) != 1 {
		return nil, errors.New("markup must contain exactly one image")
	}
	return images[0], nil
}

func parseImageCount(raw any) (int, error) {
	if raw == nil || strings.TrimSpace(strAny(raw, "")) == "" {
		return 1, nil
	}
	var count int
	switch value := raw.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0, errors.New("n must be an integer")
		}
		count = int(value)
	case json.Number:
		parsed, err := strconv.Atoi(string(value))
		if err != nil {
			return 0, errors.New("n must be an integer")
		}
		count = parsed
	case int:
		count = value
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, errors.New("n must be an integer")
		}
		count = parsed
	default:
		return 0, errors.New("n must be an integer")
	}
	if count == 0 {
		return 1, nil
	}
	if count < 1 || count > 4 {
		return 0, errors.New("n must be between 1 and 4")
	}
	return count, nil
}

func readImageSource(ctx context.Context, value any, remaining, totalBytes *int) ([][]byte, error) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, nil
		}
		if *remaining == 0 {
			return nil, fmt.Errorf("at most %d image inputs are supported", maxImageInputs)
		}
		*remaining--
		if strings.HasPrefix(strings.ToLower(text), "http://") || strings.HasPrefix(strings.ToLower(text), "https://") {
			data, err := fetchPublicImageURL(ctx, text)
			if err != nil {
				return nil, err
			}
			if err := addImageInputBytes(data, totalBytes); err != nil {
				return nil, err
			}
			return [][]byte{data}, nil
		}
		data, err := decodeImageData(text)
		if err != nil {
			return nil, err
		}
		if err := addImageInputBytes(data, totalBytes); err != nil {
			return nil, err
		}
		return [][]byte{data}, nil
	}
	if list, ok := value.([]any); ok {
		if len(list) > *remaining {
			return nil, fmt.Errorf("at most %d image inputs are supported", maxImageInputs)
		}
		out := make([][]byte, 0, len(list))
		for _, item := range list {
			inputs, err := readImageSource(ctx, item, remaining, totalBytes)
			if err != nil {
				return nil, err
			}
			out = append(out, inputs...)
		}
		return out, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid image reference")
	}
	if strings.TrimSpace(strAny(object["file_id"], "")) != "" {
		return nil, errors.New("file_id image references are not supported; use image_url instead")
	}
	inline := firstNonEmpty(strings.TrimSpace(strAny(object["b64_json"], "")), strings.TrimSpace(strAny(object["base64"], "")))
	if inline != "" {
		mediaType := firstNonEmpty(strings.TrimSpace(strAny(object["mime_type"], "")), strings.TrimSpace(strAny(object["mimeType"], "")))
		if mediaType != "" && !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			return nil, errors.New("image_url must point to an image")
		}
		return readImageSource(ctx, inline, remaining, totalBytes)
	}
	imageURL := object["image_url"]
	if imageURL == nil {
		imageURL = object["url"]
	}
	if nested, ok := imageURL.(map[string]any); ok {
		imageURL = nested["url"]
	}
	if imageURL == nil {
		return nil, errors.New("image reference must include image_url")
	}
	return readImageSource(ctx, imageURL, remaining, totalBytes)
}

func addImageInputBytes(data []byte, totalBytes *int) error {
	if len(data) == 0 {
		return errors.New("image file is empty")
	}
	if len(data) > maxImageReferenceBytes {
		return errors.New("image input exceeds 50MB limit")
	}
	*totalBytes += len(data)
	if *totalBytes > maxImageInputBytes {
		return errors.New("combined image inputs exceed 100MB limit")
	}
	return nil
}

func decodeImageData(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return decodeImageDataURL(raw)
	}
	if len(raw) > ((maxImageReferenceBytes+2)/3)*4+4 {
		return nil, errors.New("image URL exceeds 50MB limit")
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		data, err := encoding.DecodeString(raw)
		if err == nil {
			if len(data) == 0 {
				return nil, errors.New("image file is empty")
			}
			if len(data) > maxImageReferenceBytes {
				return nil, errors.New("image URL exceeds 50MB limit")
			}
			return data, nil
		}
	}
	return nil, errors.New("invalid base64 image data")
}

func decodeImageDataURL(raw string) ([]byte, error) {
	header, payload, ok := strings.Cut(raw, ",")
	if !ok {
		return nil, errors.New("invalid data image URL")
	}
	if len(payload) > maxImageReferenceBytes*2 {
		return nil, errors.New("image URL exceeds 50MB limit")
	}
	base64Payload := strings.HasSuffix(strings.ToLower(header), ";base64")
	mediaTypeHeader := strings.TrimPrefix(header, "data:")
	if base64Payload {
		mediaTypeHeader = mediaTypeHeader[:len(mediaTypeHeader)-len(";base64")]
	}
	mediaType := "image/png"
	var err error
	if mediaTypeHeader != "" {
		mediaType, _, err = mime.ParseMediaType(mediaTypeHeader)
		if err != nil {
			return nil, errors.New("invalid data image URL")
		}
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return nil, errors.New("image_url must point to an image")
	}
	var data []byte
	if base64Payload {
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			data, err = encoding.DecodeString(payload)
			if err == nil {
				break
			}
		}
	} else {
		var decoded string
		decoded, err = url.PathUnescape(payload)
		data = []byte(decoded)
	}
	if err != nil {
		return nil, errors.New("invalid data image URL")
	}
	if len(data) == 0 {
		return nil, errors.New("image URL is empty")
	}
	if len(data) > maxImageReferenceBytes {
		return nil, errors.New("image URL exceeds 50MB limit")
	}
	return data, nil
}

func validateImageInputSet(inputs [][]byte) error {
	if len(inputs) == 0 {
		return errors.New("image file or image_url is required")
	}
	if len(inputs) > maxImageInputs {
		return fmt.Errorf("at most %d image inputs are supported", maxImageInputs)
	}
	total := 0
	for _, data := range inputs {
		if len(data) == 0 {
			return errors.New("image file is empty")
		}
		if len(data) > maxImageReferenceBytes {
			return errors.New("image input exceeds 50MB limit")
		}
		total += len(data)
		if total > maxImageInputBytes {
			return errors.New("combined image inputs exceed 100MB limit")
		}
	}
	return nil
}

func readLimitedImage(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxImageReferenceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("image file is empty")
	}
	if len(data) > maxImageReferenceBytes {
		return nil, errors.New("image file exceeds 50MB limit")
	}
	return data, nil
}

func fetchPublicImageURL(ctx context.Context, raw string) ([]byte, error) {
	select {
	case imageFetchSlots <- struct{}{}:
		defer func() { <-imageFetchSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	current := strings.TrimSpace(raw)
	for redirect := 0; redirect <= maxImageRedirects; redirect++ {
		parsed, addresses, err := resolvePublicImageURL(ctx, current)
		if err != nil {
			return nil, err
		}
		transport := &http.Transport{
			Proxy:       nil,
			DialContext: pinnedImageDialer(addresses),
		}
		client := &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, errors.New("invalid image_url")
		}
		request.Header.Set("Accept", "image/*,*/*;q=0.8")
		request.Header.Set("User-Agent", "chatgpt2api image fetcher")
		response, err := client.Do(request)
		transport.CloseIdleConnections()
		if err != nil {
			return nil, errors.New("image_url fetch failed")
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := strings.TrimSpace(response.Header.Get("Location"))
			response.Body.Close()
			if location == "" {
				return nil, errors.New("image_url redirect is missing a location")
			}
			if redirect == maxImageRedirects {
				return nil, errors.New("image_url has too many redirects")
			}
			resolved, err := parsed.Parse(location)
			if err != nil {
				return nil, errors.New("invalid image_url redirect")
			}
			current = resolved.String()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			response.Body.Close()
			return nil, fmt.Errorf("image_url fetch failed: HTTP %d", response.StatusCode)
		}
		if response.ContentLength > maxImageReferenceBytes {
			response.Body.Close()
			return nil, errors.New("image URL exceeds 50MB limit")
		}
		data, err := readLimitedImage(response.Body)
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if contentType != "" && contentType != "application/octet-stream" && contentType != "binary/octet-stream" && !strings.HasPrefix(contentType, "image/") {
			return nil, errors.New("image_url must point to an image")
		}
		if contentType == "" || contentType == "application/octet-stream" || contentType == "binary/octet-stream" {
			if guessed := mime.TypeByExtension(path.Ext(parsed.Path)); guessed == "" || !strings.HasPrefix(guessed, "image/") {
				return nil, errors.New("image_url must point to an image")
			}
		}
		return data, nil
	}
	return nil, errors.New("image_url has too many redirects")
}

func resolvePublicImageURL(ctx context.Context, raw string) (*url.URL, []net.IP, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, nil, errors.New("image_url must be an http or https URL")
	}
	if parsed.User != nil {
		return nil, nil, errors.New("image_url must not include credentials")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return nil, nil, errors.New("image_url host is not publicly reachable")
	}
	port := parsed.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[parsed.Scheme == "https"]
	}
	if literal := net.ParseIP(host); literal != nil {
		if !isPublicImageIP(literal) {
			return nil, nil, errors.New("image_url host is not publicly reachable")
		}
		return parsed, []net.IP{literal}, nil
	}
	resolvedEntries, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	resolved := make([]net.IP, 0, len(resolvedEntries))
	for _, entry := range resolvedEntries {
		resolved = append(resolved, entry.IP)
	}
	if err != nil || len(resolved) == 0 {
		return nil, nil, errors.New("image_url host could not be resolved")
	}
	addresses := make([]net.IP, 0, len(resolved))
	for _, ip := range resolved {
		if !isPublicImageIP(ip) {
			return nil, nil, errors.New("image_url host is not publicly reachable")
		}
		addresses = append(addresses, ip)
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), port)
	return parsed, addresses, nil
}

func isPublicImageIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, network := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"2001:db8::/32", "2001::/32",
	} {
		if netip.MustParsePrefix(network).Contains(address) {
			return false
		}
	}
	return true
}

func pinnedImageDialer(addresses []net.IP) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 30 * time.Second}
		var lastErr error
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
