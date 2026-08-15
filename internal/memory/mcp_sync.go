package memory

// mcp_sync.go — dual-write авто-извлечённых фактов в общий memory-mcp
// (SQLite+FTS5, канон ~/original/custom/memory-mcp/memory_mcp.py).
//
// Фаза 2 (2026-08-15): нативная запись остаётся (Store.SaveWithOptions),
// параллельно факты пишутся в memory-mcp (shared store, кросс-рантайм).
//
// Включение: REASONIX_MEMORY_MCP=1 (иначе всё — no-op).
//   MEMORY_MCP_CMD — команда сервера (default /home/<user>/.local/bin/memory-mcp)
//   MEMORY_MCP_DB  — путь БД (default ~/shared-store/facts.db;
//                    в docker-рантаймах задаётся bind-mount'ом).
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
	"strings"
	"time"
)

const (
	mcpDefaultCmd  = "/home/<user>/.local/bin/memory-mcp"
	mcpDefaultDB   = "/home/<user>/shared-store/facts.db"
	mcpSyncTimeout = 60 * time.Second
)

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
	cmdStr := os.Getenv("MEMORY_MCP_CMD")
	if cmdStr == "" {
		cmdStr = mcpDefaultCmd
	}
	db := os.Getenv("MEMORY_MCP_DB")
	if db == "" {
		db = mcpDefaultDB
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpSyncTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdStr)
	cmd.Env = append(os.Environ(), "MEMORY_MCP_DB="+db)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp start: %w", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)

	if err := writeMCPRPC(stdin, 1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "reasonix-memory-sync", "version": "1"},
	}); err != nil {
		return err
	}
	if _, err := waitMCPRPC(sc, 1); err != nil {
		return fmt.Errorf("mcp init: %w (stderr: %s)", err, stderr.String())
	}

	id := 1
	for _, m := range memories {
		text := strings.TrimSpace(firstNonEmpty(m.Body, m.Description, m.Title))
		if text == "" {
			continue
		}
		id++
		args := map[string]any{
			"text":    text,
			"source":  source,
			"trust":   "medium",
			"domain":  NormalizeType(string(m.Type)),
			"project": string(m.Scope),
			"strong":  m.Trust == TrustHigh,
		}
		if err := writeMCPRPC(stdin, id, "tools/call", map[string]any{
			"name": "remember_fact", "arguments": args,
		}); err != nil {
			return err
		}
		if _, err := waitMCPRPC(sc, id); err != nil {
			head := text
			if len(head) > 40 {
				head = head[:40]
			}
			return fmt.Errorf("mcp remember(%s): %w (stderr: %s)", head, err, stderr.String())
		}
	}
	return nil
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
