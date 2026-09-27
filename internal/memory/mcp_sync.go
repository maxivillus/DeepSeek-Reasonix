package memory

// mcp_sync.go — dual-write авто-извлечённых фактов в общий memory-mcp
// (SQLite+FTS5; сервер: github.com/maxivillus/memory-mcp).
//
// Фаза 2 (2026-08-15): нативная запись остаётся (Store.SaveWithOptions),
// параллельно факты пишутся в memory-mcp (shared store, кросс-рантайм).
//
// Включение: REASONIX_MEMORY_MCP=1 (иначе всё — no-op).
//   MEMORY_MCP_CMD — команда сервера (default "memory-mcp" из PATH)
//   MEMORY_MCP_DB  — путь БД (default ~/.local/share/memory-mcp/facts.db;
//                    XDG-стиль, без хостовых путей; в рантаймах стека
//                    задаётся явно — общая БД через bind-mount).
//
// Запись best-effort: при любой ошибке — stderr + return error; вызывающие
// игнорируют (нативная запись уже выполнена, MCP-синк не блокирует экстракцию).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// mcpDefaultCmd resolves via PATH on every machine; the host stack overrides
	// with MEMORY_MCP_CMD=/home/<user>/.local/bin/memory-mcp.
	mcpDefaultCmd  = "memory-mcp"
	mcpSyncTimeout = 60 * time.Second
)

// mcpDefaultDB returns an XDG-style user data path (no host paths baked in).
func mcpDefaultDB() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "memory-mcp", "facts.db")
	}
	return "memory-mcp-facts.db" // last resort: relative to CWD
}

func mcpSyncEnabled() bool { return os.Getenv("REASONIX_MEMORY_MCP") == "1" }

type mcpRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// SyncExtractedFactsToMCP — convenience: строит Memory из ExtractFact (та же
// логика, что SaveExtractedFacts) и пишет батчем в memory-mcp.
func SyncExtractedFactsToMCP(facts []ExtractFact, mode, source string) error {
	if !mcpSyncEnabled() {
		return nil
	}
	return SyncFactsToMCP(buildExtractedMemories(facts, mode), source)
}

// SyncFactsToMCP пишет список фактов в memory-mcp одним запуском сервера.
// trust: high только для TrustHigh (пользовательски подтверждённые); у
// авто-экстракции TrustMedium → medium, strong=false.
func SyncFactsToMCP(memories []Memory, source string) error {
	if !mcpSyncEnabled() || len(memories) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpSyncTimeout)
	defer cancel()

	sess, err := startMCPSession(ctx)
	if err != nil {
		return err
	}
	defer sess.close()

	for _, m := range memories {
		text := strings.TrimSpace(firstNonEmpty(m.Body, m.Description, m.Title))
		if text == "" {
			continue
		}
		args := map[string]any{
			"text":    text,
			"source":  source,
			"trust":   "medium",
			"domain":  NormalizeType(string(m.Type)),
			"project": string(m.Scope),
			"strong":  m.Trust == TrustHigh,
		}
		if _, err := sess.call(ctx, "tools/call", map[string]any{
			"name": "remember_fact", "arguments": args,
		}); err != nil {
			head := text
			if len(head) > 40 {
				head = head[:40]
			}
			return fmt.Errorf("mcp remember(%s): %w", head, err)
		}
	}
	return nil
}

// mcpSession is one stdio MCP server session (newline-delimited JSON-RPC 2.0).
// Write (remember_fact) and read (summarize_index, search_facts) paths share
// it so both reuse the same spawn/init/call plumbing.
type mcpSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	sc     *bufio.Scanner
	stderr strings.Builder
	nextID int
}

// startMCPSession spawns the server (MEMORY_MCP_CMD, fallback "memory-mcp" from
// PATH; DB via MEMORY_MCP_DB, fallback XDG path) and completes initialize. The
// caller must close() the session when done.
func startMCPSession(ctx context.Context) (*mcpSession, error) {
	cmdStr := os.Getenv("MEMORY_MCP_CMD")
	if cmdStr == "" {
		cmdStr = mcpDefaultCmd
	}
	db := os.Getenv("MEMORY_MCP_DB")
	if db == "" {
		db = mcpDefaultDB()
	}
	cmd := exec.CommandContext(ctx, cmdStr)
	cmd.Env = append(os.Environ(), "MEMORY_MCP_DB="+db)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	sess := &mcpSession{cmd: cmd, stdin: stdin, nextID: 1}
	sess.sc = bufio.NewScanner(stdout)
	sess.sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	cmd.Stderr = &sess.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp start: %w", err)
	}
	if err := sess.init(); err != nil {
		sess.close()
		return nil, err
	}
	return sess, nil
}

func (s *mcpSession) init() error {
	if err := writeMCPRPC(s.stdin, s.nextID, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "reasonix-memory", "version": "1"},
	}); err != nil {
		return err
	}
	id := s.nextID
	s.nextID++
	if _, err := waitMCPRPC(s.sc, id); err != nil {
		return fmt.Errorf("mcp init: %w (stderr: %s)", err, s.stderr.String())
	}
	return nil
}

// call performs one RPC and returns the raw result object. For tools/call the
// result is {"content":[{"type":"text","text":"<json>"}]}.
func (s *mcpSession) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	id := s.nextID
	s.nextID++
	if err := writeMCPRPC(s.stdin, id, method, params); err != nil {
		return nil, err
	}
	res, err := waitMCPRPC(s.sc, id)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w (stderr: %s)", method, err, s.stderr.String())
	}
	return res, nil
}

func (s *mcpSession) close() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}

func writeMCPRPC(w io.Writer, id int, method string, params any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	_, err = w.Write(append(payload, '\n'))
	return err
}

func waitMCPRPC(sc *bufio.Scanner, want int) (json.RawMessage, error) {
	for sc.Scan() {
		var msg mcpRPC
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		if msg.ID != want {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("rpc %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return msg.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("server closed before id %d", want)
}
