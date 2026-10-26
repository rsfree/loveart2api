// Package sign 用内嵌的 signature_wasm 计算 Lovart 网关所需的 X-Client-Signature。
//
// 上游把签名算法编成了 WebAssembly（Rust + hmac/sha2），前端在每次请求前用
// (timestamp, reqUuid) 调 wasm 得到形如 "1:<hex>" 的签名。这里用 wazero 直接跑同一份
// wasm，避免复刻算法——上游换算法时只需要替换 assets/signature.wasm。
package sign

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed signature.wasm
var wasmBin []byte

// Signer 是线程安全的：wazero 的 Module 实例内部有线性内存，调用需要串行。
type Signer struct {
	mu     sync.Mutex
	rt     wazero.Runtime
	mod    api.Module
	malloc api.Function
	stack  api.Function
	gs     api.Function
	mem    api.Memory
}

// New 加载并实例化签名 wasm。上游的 wasm 没有宿主依赖（import 只有空的 wbg 命名空间），
// 所以不需要注册任何宿主函数。
func New(ctx context.Context) (*Signer, error) {
	rt := wazero.NewRuntime(ctx)
	mod, err := rt.InstantiateWithConfig(ctx, wasmBin, wazero.NewModuleConfig().WithName("lovart_sign"))
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate signature wasm: %w", err)
	}
	s := &Signer{rt: rt, mod: mod}
	for name, dst := range map[string]*api.Function{
		"__wbindgen_export2":           &s.malloc,
		"__wbindgen_add_to_stack_pointer": &s.stack,
		"gs":                           &s.gs,
	} {
		fn := mod.ExportedFunction(name)
		if fn == nil {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("signature wasm 缺少导出函数 %s", name)
		}
		*dst = fn
	}
	s.mem = mod.Memory()
	if s.mem == nil {
		_ = rt.Close(ctx)
		return nil, errors.New("signature wasm 没有导出内存")
	}
	return s, nil
}

// Close 释放 wasm 运行时。
func (s *Signer) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rt.Close(ctx)
}

// Sign 返回 "1:<hex>" 形式的客户端签名。参数与前端一致：毫秒时间戳 + 去横线的 uuid。
func (s *Signer) Sign(ctx context.Context, timestamp, reqUUID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sp, err := s.stack.Call(ctx, api.EncodeI32(-16))
	if err != nil {
		return "", fmt.Errorf("wasm stack alloc: %w", err)
	}
	defer func() {
		_, _ = s.stack.Call(ctx, api.EncodeI32(16))
	}()
	stackPtr := uint32(sp[0])

	p1, err := s.writeString(ctx, timestamp)
	if err != nil {
		return "", err
	}
	p2, err := s.writeString(ctx, reqUUID)
	if err != nil {
		return "", err
	}
	p3, err := s.writeString(ctx, "")
	if err != nil {
		return "", err
	}
	p4, err := s.writeString(ctx, "")
	if err != nil {
		return "", err
	}

	// gs(stackPtr, ts.ptr, ts.len, uuid.ptr, uuid.len, a.ptr, a.len, b.ptr, b.len)
	if _, err := s.gs.Call(ctx, api.EncodeU32(stackPtr),
		api.EncodeU32(p1.ptr), api.EncodeU32(p1.length),
		api.EncodeU32(p2.ptr), api.EncodeU32(p2.length),
		api.EncodeU32(p3.ptr), api.EncodeU32(p3.length),
		api.EncodeU32(p4.ptr), api.EncodeU32(p4.length),
	); err != nil {
		return "", fmt.Errorf("wasm gs: %w", err)
	}

	raw, ok := s.mem.Read(stackPtr, 8)
	if !ok {
		return "", errors.New("wasm 读取返回值指针失败")
	}
	outPtr := binary.LittleEndian.Uint32(raw[0:4])
	outLen := binary.LittleEndian.Uint32(raw[4:8])
	if outLen == 0 || outLen > 4096 {
		return "", fmt.Errorf("wasm 返回长度异常: %d", outLen)
	}
	buf, ok := s.mem.Read(outPtr, outLen)
	if !ok {
		return "", errors.New("wasm 读取签名字节失败")
	}
	return string(buf), nil
}

type ptrLen struct {
	ptr    uint32
	length uint32
}

// writeString 把字符串拷进 wasm 线性内存，返回 (ptr, len)。
func (s *Signer) writeString(ctx context.Context, v string) (ptrLen, error) {
	b := []byte(v)
	res, err := s.malloc.Call(ctx, api.EncodeU32(uint32(len(b))), api.EncodeU32(1))
	if err != nil {
		return ptrLen{}, fmt.Errorf("wasm malloc: %w", err)
	}
	p := uint32(res[0])
	if len(b) > 0 && !s.mem.Write(p, b) {
		return ptrLen{}, errors.New("wasm 写入字符串失败")
	}
	return ptrLen{ptr: p, length: uint32(len(b))}, nil
}
