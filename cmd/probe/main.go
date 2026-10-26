// probe 是 Lovart 上游链路的诊断工具：验鉴权、探模型目录、找项目、试上传，
// 可选跑一次真实生成。用于部署前自查与上游改版后的快速定位。
//
//	probe -token-file ./token.txt                # 只读探测（不消耗额度）
//	probe -token-file ./token.txt -generate      # 额外跑一次图生图（消耗额度）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/sign"
)

func main() {
	var (
		tokenFile = flag.String("token-file", "", "包含 usertoken 的文件（单行）")
		token     = flag.String("token", "", "直接给 usertoken（不推荐，会留在 shell history）")
		kitID     = flag.String("kit", "", "复用已有 kit_id")
		generator = flag.String("generator", "vertex/anon-bob", "要探测的 generator_name")
		prompt    = flag.String("prompt", "a single red circle on pure white background", "生成提示词")
		imagePath = flag.String("image", "", "图生图的输入图路径；留空则走文生图")
		doGen     = flag.Bool("generate", false, "是否真的提交一次生成任务（消耗额度）")
		wwwBase   = flag.String("www", lovart.DefaultWWWBase, "www 基址")
		lgwBase   = flag.String("lgw", lovart.DefaultLGWBase, "lgw 基址")
	)
	flag.Parse()

	tk := strings.TrimSpace(*token)
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		must(err, "读取 token 文件")
		tk = strings.TrimSpace(string(b))
	}
	if tk == "" {
		fmt.Fprintln(os.Stderr, "缺少 token：用 -token-file 指定包含 usertoken 的单行文件")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sg, err := sign.New(ctx)
	must(err, "加载签名 wasm")
	defer sg.Close(ctx)
	fmt.Printf("✓ 签名 wasm 就绪（%d bytes 内嵌）\n", 41232)

	c := lovart.NewClient(tk, sg)
	c.WWWBase, c.LGWBase = *wwwBase, *lgwBase

	// 1) 模型目录
	gens, err := c.ListGenerators(ctx)
	if err != nil {
		fail("拉模型目录失败", err)
	}
	var images, videos int
	for _, g := range gens {
		switch g.Type {
		case "image", "image-modify":
			images++
		case "video", "video-modify":
			videos++
		}
	}
	fmt.Printf("✓ 模型目录：共 %d 个（image %d / video %d）\n", len(gens), images, videos)
	for _, g := range gens {
		if g.Type == "image" || g.Type == "image-modify" {
			fmt.Printf("    %-32s %-22s\n", g.Name, g.DisplayName)
		}
	}

	// 2) 项目
	pid, err := c.ResolveProjectID(ctx)
	if err != nil {
		fail("定位 project_id 失败", err)
	}
	fmt.Printf("✓ project_id = %s\n", pid)

	// 3) kit
	kit, err := c.EnsureKit(ctx, *kitID, "lovart2api")
	if err != nil {
		fail("准备 kit 失败", err)
	}
	fmt.Printf("✓ kit_id = %s\n", kit)

	// 4) 上传
	var imageURL string
	if *imagePath != "" {
		data, err := os.ReadFile(*imagePath)
		must(err, "读取输入图")
		imageURL, err = c.UploadImage(ctx, kit, *imagePath, data)
		if err != nil {
			fail("上传图片失败", err)
		}
		fmt.Printf("✓ 上传成功：%s\n", imageURL)
	}

	if !*doGen {
		fmt.Println("（未加 -generate，跳过真实生成）")
		return
	}

	// 5) 生成
	input := map[string]any{"prompt": *prompt, "aspect_ratio": "1:1"}
	if imageURL != "" {
		input["image"] = []string{imageURL}
		input["resolution"] = "1K"
	}
	start := time.Now()
	taskID, err := c.CreateTask(ctx, pid, *generator, input)
	if err != nil {
		fail("提交任务失败", err)
	}
	fmt.Printf("✓ 任务已提交：%s（%s）\n", taskID, time.Since(start).Round(time.Millisecond))

	t, err := c.WaitTask(ctx, taskID, time.Second, 5*time.Second)
	if err != nil {
		fail("任务未完成", err)
	}
	fmt.Printf("✓ 任务完成：status=%s 用时=%s\n", t.Status, time.Since(start).Round(time.Millisecond))
	for _, u := range t.ImageURLs() {
		fmt.Printf("    产物 %s\n", u)
	}
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %s: %v\n", what, err)
		os.Exit(1)
	}
}

func fail(what string, err error) {
	var ae *lovart.APIError
	if e, ok := err.(*lovart.APIError); ok {
		ae = e
	}
	if ae != nil {
		fmt.Fprintf(os.Stderr, "✗ %s: kind=%s code=%s msg=%s\n", what, ae.Kind, ae.Code, ae.Message)
	} else {
		fmt.Fprintf(os.Stderr, "✗ %s: %v\n", what, err)
	}
	os.Exit(1)
}
