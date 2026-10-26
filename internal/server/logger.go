package server

import (
	"sync"
	"time"
)

// Entry 一次请求的记录。
type Entry struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Model    string    `json:"model,omitempty"`
	Account  string    `json:"account,omitempty"`
	Status   int       `json:"status"`
	Millis   int64     `json:"ms"`
	TaskID   string    `json:"task_id,omitempty"`
	Images   int       `json:"images,omitempty"`
	Error    string    `json:"error,omitempty"`
	RemoteIP string    `json:"remote_ip,omitempty"`
	UA       string    `json:"ua,omitempty"`
}

// Logger 固定容量的请求日志环。
type Logger struct {
	mu      sync.Mutex
	entries []Entry
	cap     int
	total   int64
}

// NewLogger 构造日志环。
func NewLogger(capacity int) *Logger {
	if capacity <= 0 {
		capacity = 1000
	}
	return &Logger{cap: capacity}
}

// Add 追加一条。
func (l *Logger) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	l.entries = append(l.entries, e)
	if len(l.entries) > l.cap {
		l.entries = l.entries[len(l.entries)-l.cap:]
	}
}

// Count 累计请求数（不受环容量限制）。
func (l *Logger) Count() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Recent 取最近 n 条（新的在前）。
func (l *Logger) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	out := make([]Entry, 0, n)
	for i := len(l.entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.entries[i])
	}
	return out
}
