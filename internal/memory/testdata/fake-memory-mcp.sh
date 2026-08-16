#!/bin/sh
# Fake memory-mcp server for internal/memory tests (newline-delimited JSON-RPC).
# Answers initialize + tools/call for summarize_index and search_facts with
# canned data about a single shared fact (#42, "quantum widgets").
while IFS= read -r line || [ -n "$line" ]; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  [ -n "$id" ] || id=1
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake-memory-mcp","version":"test"}}}\n' "$id"
      ;;
    *'"name":"summarize_index"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"count\":1,\"total\":1,\"chars\":48,\"truncated\":false,\"index\":\"#42 high! [project] Fake shared fact about quantum widgets\"}"}]}}\n' "$id"
      ;;
    *'"name":"search_facts"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"count\":1,\"facts\":[{\"id\":42,\"text\":\"Fake shared fact about quantum widgets\",\"source\":\"test\",\"project\":\"project\",\"domain\":\"project\",\"trust\":\"high\",\"strong\":true,\"created_at\":\"2026-08-16T00:00:00Z\",\"updated_at\":\"2026-08-16T00:00:00Z\"}]}"}]}}\n' "$id"
      ;;
  esac
done
