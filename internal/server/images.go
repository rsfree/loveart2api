package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/config"
	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
)

// maxImageBytes 单张输入图上限（上游 max_size 为 50MB，这里留余量）。
const maxImageBytes = 32 << 20

// generationsRequest 是 /v1/images/generations 的请求体。
type generationsRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
	ResponseFormat string `json:"response_format"`
	Seed           int64  `json:"seed"`
	NegativePrompt string `json:"negative_prompt"`
	// Image 允许在 generations 里带输入图（等价于 edits，方便统一调用）
	Image any `json:"image"`
}

// genRequest 是内部统一后的请求。
type genRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	AspectRatio    string
	Resolution     string
	ResponseFormat string
	Seed           int64
	NegativePrompt string
	Images         []imageInput
}

// imageInput 一个输入图。
type imageInput struct {
	// Data 为空时表示 URL 直用（lovart 自家产物）
	Data []byte
	Name string
	URL  string
}

func (s *Server) handleGenerations(w http.ResponseWriter, r *http.Request) {
	var req generationsRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	gen := genRequest{
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              req.N,
		Size:           req.Size,
		AspectRatio:    req.AspectRatio,
		Resolution:     req.Resolution,
		ResponseFormat: req.ResponseFormat,
		Seed:           req.Seed,
		NegativePrompt: req.NegativePrompt,
	}
	// generations 里带 image 时按图生图处理
	if req.Image != nil {
		imgs, err := decodeImageField(r.Context(), req.Image)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_image", err.Error())
			return
		}
		gen.Images = imgs
	}
	s.runGenerate(w, r, gen)
}

func (s *Server) handleEdits(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		s.handleEditsMultipart(w, r)
		return
	}
	// JSON 形态：{"image": "data:image/png;base64,…" | ["…"], "prompt": "…"}
	var body struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		Image          any    `json:"image"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		AspectRatio    string `json:"aspect_ratio"`
		Resolution     string `json:"resolution"`
		ResponseFormat string `json:"response_format"`
		Seed           int64  `json:"seed"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if body.Image == nil {
		writeErr(w, http.StatusBadRequest, "missing_image", "edits 需要 image 字段")
		return
	}
	imgs, err := decodeImageField(r.Context(), body.Image)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_image", err.Error())
		return
	}
	s.runGenerate(w, r, genRequest{
		Model: body.Model, Prompt: body.Prompt, N: body.N, Size: body.Size,
		AspectRatio: body.AspectRatio, Resolution: body.Resolution,
		ResponseFormat: body.ResponseFormat, Seed: body.Seed,
		NegativePrompt: body.NegativePrompt, Images: imgs,
	})
}

func (s *Server) handleEditsMultipart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_form", "解析 multipart 失败: "+err.Error())
		return
	}
	gen := genRequest{
		Model:          r.FormValue("model"),
		Prompt:         firstNonEmpty(r.FormValue("prompt"), r.FormValue("text")),
		Size:           r.FormValue("size"),
		AspectRatio:    r.FormValue("aspect_ratio"),
		Resolution:     r.FormValue("resolution"),
		ResponseFormat: r.FormValue("response_format"),
		NegativePrompt: r.FormValue("negative_prompt"),
		N:              parseIntDefault(r.FormValue("n"), 1),
	}
	if v := r.FormValue("seed"); v != "" {
		gen.Seed, _ = strconv.ParseInt(v, 10, 64)
	}

	// 兼容 image / image[] / image_1…image_9 三种命名
	files := r.MultipartForm.File["image"]
	files = append(files, r.MultipartForm.File["image[]"]...)
	for i := 1; i <= 9; i++ {
		if f := r.MultipartForm.File["image_"+strconv.Itoa(i)]; len(f) > 0 {
			files = append(files, f...)
		}
	}
	// 也接受 JSON 里的 base64 字符串字段
	if len(files) == 0 {
		if v := r.FormValue("image"); v != "" {
			imgs, err := decodeImageField(r.Context(), v)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_image", err.Error())
				return
			}
			gen.Images = imgs
		}
	}
	for _, fh := range files {
		if fh.Size > maxImageBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "image_too_large", fmt.Sprintf("图片 %s 超过 %d 字节", fh.Filename, maxImageBytes))
			return
		}
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_image", "打开上传文件失败: "+err.Error())
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_image", "读取上传文件失败: "+err.Error())
			return
		}
		gen.Images = append(gen.Images, imageInput{Data: data, Name: fh.Filename})
	}
	if len(gen.Images) == 0 {
		writeErr(w, http.StatusBadRequest, "missing_image", "edits 需要至少一张输入图（image 字段）")
		return
	}
	s.runGenerate(w, r, gen)
}

// decodeImageField 解析 image 字段：字符串、数组、data URL、http URL、裸 base64。
func decodeImageField(ctx context.Context, v any) ([]imageInput, error) {
	var items []string
	switch t := v.(type) {
	case string:
		items = []string{t}
	case []any:
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New("image 数组里只能放字符串")
			}
			items = append(items, s)
		}
	default:
		return nil, errors.New("image 必须是字符串或字符串数组")
	}
	out := make([]imageInput, 0, len(items))
	for i, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		switch {
		case strings.HasPrefix(it, "data:"):
			// data:image/png;base64,xxxx
			idx := strings.Index(it, ",")
			if idx < 0 {
				return nil, fmt.Errorf("第 %d 张图的 data URL 格式不对", i+1)
			}
			raw, err := base64.StdEncoding.DecodeString(it[idx+1:])
			if err != nil {
				return nil, fmt.Errorf("第 %d 张图 base64 解码失败: %w", i+1, err)
			}
			out = append(out, imageInput{Data: raw, Name: fmt.Sprintf("input-%d.png", i+1)})
		case strings.HasPrefix(it, "http://"), strings.HasPrefix(it, "https://"):
			// lovart 自家产物可直接复用，省一次下载+上传
			if isLovartArtifact(it) {
				out = append(out, imageInput{URL: it})
				continue
			}
			data, _, err := downloadImage(ctx, it)
			if err != nil {
				return nil, fmt.Errorf("下载第 %d 张图失败: %w", i+1, err)
			}
			out = append(out, imageInput{Data: data, Name: fmt.Sprintf("input-%d.png", i+1)})
		default:
			raw, err := base64.StdEncoding.DecodeString(it)
			if err != nil {
				return nil, fmt.Errorf("第 %d 张图既不是 URL 也不是合法 base64", i+1)
			}
			out = append(out, imageInput{Data: raw, Name: fmt.Sprintf("input-%d.png", i+1)})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("没有解析到有效图片")
	}
	return out, nil
}

// isLovartArtifact 判断是否 lovart 自家全局产物（可跨账号复用）。
func isLovartArtifact(u string) bool {
	return strings.HasPrefix(u, "https://a.lovart.ai/artifacts/")
}

// downloadImage 下载外部图片。
func downloadImage(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("user-agent", "lovart2api/1.0")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("图片超过 %d 字节", maxImageBytes)
	}
	return data, resp.Header.Get("content-type"), nil
}

// resolveSpec 把请求里的 model 解析成上游映射。
func (s *Server) resolveSpec(model string, isEdit bool) (string, config.ModelSpec, error) {
	if model == "" {
		if isEdit {
			model = s.Cfg.Images.DefaultEditModel
		} else {
			model = s.Cfg.Images.DefaultModel
		}
	}
	if spec, ok := s.Cfg.Models[model]; ok {
		if isEdit && !spec.SupportsEdit {
			return "", spec, fmt.Errorf("模型 %s 不支持图生图", model)
		}
		return model, spec, nil
	}
	// 允许直接传上游 generator 名
	for name, spec := range s.Cfg.Models {
		if spec.Generator == model {
			if isEdit && !spec.SupportsEdit {
				return "", spec, fmt.Errorf("模型 %s 不支持图生图", model)
			}
			return name, spec, nil
		}
	}
	return "", config.ModelSpec{}, fmt.Errorf("未知模型 %q（可用见 /v1/models）", model)
}

// buildInput 把内部请求翻成上游 input_args。
func buildInput(gen genRequest, spec config.ModelSpec, imageURLs []string) map[string]any {
	input := map[string]any{"prompt": gen.Prompt}
	if len(imageURLs) > 0 {
		if spec.SingleImage {
			input[spec.ImageField] = imageURLs[0]
		} else {
			field := spec.ImageField
			if field == "" {
				field = "image"
			}
			input[field] = imageURLs
		}
	}
	for k, v := range spec.Extra {
		input[k] = v
	}
	ar := gen.AspectRatio
	if ar == "" && gen.Size != "" {
		ar = sizeToAspect(gen.Size)
	}
	if ar != "" {
		// 上游只认枚举内的比例，任意 w:h 先折算到最近的一档，
		// 否则任务能创建成功但执行时报 VALIDATION_ERROR（白烧一次额度）。
		if len(spec.AspectRatios) > 0 {
			ar = clampAspect(ar, spec.AspectRatios)
		}
		input["aspect_ratio"] = ar
	}
	if gen.Resolution != "" {
		res := gen.Resolution
		if len(spec.Resolutions) > 0 {
			res = clampEnum(res, spec.Resolutions)
		}
		input["resolution"] = res
	}
	if gen.Seed != 0 {
		input["seed"] = gen.Seed
	}
	if gen.NegativePrompt != "" {
		input["negative_prompt"] = gen.NegativePrompt
	}
	return input
}

// clampAspect 把任意比例的映射到候选集合里最接近的一档（按对数距离，横竖对称）。
func clampAspect(want string, supported []string) string {
	w, h, ok := parseRatio(want)
	if !ok {
		return supported[0]
	}
	best := supported[0]
	bestScore := 1e9
	for _, s := range supported {
		sw, sh, ok := parseRatio(s)
		if !ok {
			continue
		}
		// 用 log 比，避免 21:9 这类极端比例把距离算歪
		score := absFloat(math.Log((float64(w) / float64(h)) / (float64(sw) / float64(sh))))
		if score < bestScore {
			bestScore = score
			best = s
		}
	}
	return best
}

// clampEnum 命中枚举则原样返回，否则按数值最近匹配，无法比较时取第一个。
func clampEnum(want string, allowed []string) string {
	low := strings.ToLower(strings.TrimSpace(want))
	for _, a := range allowed {
		if strings.ToLower(a) == low {
			return a
		}
	}
	// 1K / 2K / 4K 这类：取数值最近的
	wantNum, wantOK := parseLeadingNumber(low)
	if !wantOK {
		return allowed[0]
	}
	best := allowed[0]
	bestDiff := 1e9
	for _, a := range allowed {
		n, ok := parseLeadingNumber(strings.ToLower(a))
		if !ok {
			continue
		}
		if d := absFloat(n - wantNum); d < bestDiff {
			bestDiff = d
			best = a
		}
	}
	return best
}

func parseRatio(s string) (int, int, bool) {
	i := strings.IndexAny(s, ":x×")
	if i <= 0 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
	h, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func parseLeadingNumber(s string) (float64, bool) {
	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// sizeToAspect 把 OpenAI 的 size 折成上游 aspect_ratio。
func sizeToAspect(size string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return ""
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return ""
	}
	g := gcd(w, h)
	return fmt.Sprintf("%d:%d", w/g, h/g)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// runGenerate 是核心执行链：选号 → 准备素材 → 提交 → 轮询 → 返回。
func (s *Server) runGenerate(w http.ResponseWriter, r *http.Request, gen genRequest) {
	start := time.Now()
	isEdit := len(gen.Images) > 0
	modelName, spec, err := s.resolveSpec(gen.Model, isEdit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}
	if strings.TrimSpace(gen.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "missing_prompt", "prompt 不能为空")
		return
	}

	entry := Entry{
		Time:     start,
		Method:   r.Method,
		Path:     r.URL.Path,
		Model:    modelName,
		RemoteIP: clientIP(r),
		UA:       truncateStr(r.UserAgent(), 120),
	}
	defer func() {
		entry.Millis = time.Since(start).Milliseconds()
		s.Log.Add(entry)
	}()

	timeout := config.Duration(s.Cfg.Images.Timeout, 15*time.Minute)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	maxRetries := s.Cfg.Pool.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	exclude := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		lease, err := s.Pool.Pick(ctx, exclude, spec.Generator)
		if err != nil {
			lastErr = err
			break
		}
		entry.Account = shortID(lease.Account.ID)

		_, task, err := s.execute(ctx, lease.Account, spec, gen)
		if err == nil {
			lease.Release(nil)
			entry.TaskID = task.GeneratorTaskID
			imgs := task.ImageURLs()
			entry.Images = len(imgs)
			s.writeImages(ctx, w, r, imgs, gen.ResponseFormat, entry)
			return
		}

		lease.Release(err)
		lastErr = err
		// 额度/鉴权/并发 → 换号重试；参数类错误（unsupported）直接结束
		if lovart.IsUnsupported(err) {
			break
		}
		exclude[lease.Account.ID] = true
		s.logf("[retry] model=%s account=%s attempt=%d err=%v", modelName, shortID(lease.Account.ID), attempt+1, err)
	}

	status, code, msg := mapUpstreamErr(lastErr)
	entry.Status = status
	entry.Error = msg
	writeErr(w, status, code, msg)
}

// execute 在指定账号上完成一次生成。
func (s *Server) execute(ctx context.Context, acct *pool.Account, spec config.ModelSpec, gen genRequest) ([]string, *lovart.Task, error) {
	client := lovart.NewClient(acct.Token, s.Signer)
	client.WWWBase = s.Cfg.Upstream.WWWBase
	client.LGWBase = s.Cfg.Upstream.LGWBase
	client.Lang = s.Cfg.Upstream.Lang

	projectID := acct.ProjectID
	if projectID == "" {
		pid, err := client.ResolveProjectID(ctx)
		if err != nil {
			return nil, nil, err
		}
		projectID = pid
		_ = s.Pool.Update(acct.ID, func(a *pool.Account) { a.ProjectID = pid })
	}

	// 输入图：需要上传的先上传（上传依赖账号的 kit）
	var imageURLs []string
	if len(gen.Images) > 0 {
		kitID := acct.KitID
		for _, img := range gen.Images {
			if img.URL != "" {
				imageURLs = append(imageURLs, img.URL)
				continue
			}
			if kitID == "" {
				kit, err := client.EnsureKit(ctx, "", "lovart2api")
				if err != nil {
					return nil, nil, err
				}
				kitID = kit
				_ = s.Pool.Update(acct.ID, func(a *pool.Account) { a.KitID = kit })
			}
			url, err := client.UploadImage(ctx, kitID, img.Name, img.Data)
			if err != nil {
				return nil, nil, err
			}
			imageURLs = append(imageURLs, url)
		}
	}

	input := buildInput(gen, spec, imageURLs)
	taskID, err := client.CreateTask(ctx, projectID, spec.Generator, input)
	if err != nil {
		return nil, nil, err
	}

	task, err := client.WaitTask(ctx, taskID,
		config.Duration(s.Cfg.Images.PollInterval, 1200*time.Millisecond),
		config.Duration(s.Cfg.Images.PollMax, 5*time.Second))
	if err != nil {
		return nil, task, err
	}
	urls := task.ImageURLs()
	if len(urls) == 0 {
		return nil, task, &lovart.APIError{Kind: lovart.ErrUpstream, Message: "上游任务完成但没有产物"}
	}
	return urls, task, nil
}

// writeImages 输出 OpenAI 风格的 images 响应。
func (s *Server) writeImages(ctx context.Context, w http.ResponseWriter, r *http.Request, urls []string, format string, entry Entry) {
	type datum struct {
		URL           string `json:"url,omitempty"`
		B64           string `json:"b64_json,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	resp := struct {
		Created int64   `json:"created"`
		Data    []datum `json:"data"`
	}{Created: time.Now().Unix()}

	wantB64 := strings.EqualFold(format, "b64_json") || s.Cfg.Images.DownloadResult
	for _, u := range urls {
		if wantB64 {
			// 产物在 a.lovart.ai（境外 CDN），由网关代取可让客户端少一次跨网请求
			client := lovart.NewClient("", s.Signer)
			data, _, err := client.Download(ctx, u)
			if err != nil {
				s.logf("[warn] 下载产物失败，回退 url: %v", err)
				resp.Data = append(resp.Data, datum{URL: u})
				continue
			}
			resp.Data = append(resp.Data, datum{B64: base64.StdEncoding.EncodeToString(data)})
			continue
		}
		resp.Data = append(resp.Data, datum{URL: u})
	}
	entry.Status = http.StatusOK
	writeJSON(w, http.StatusOK, resp)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
