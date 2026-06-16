package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	delay := flag.Duration("delay-initialize", 0, "delay initialize response")
	polluteStdout := flag.Bool("pollute-stdout", false, "write a non-JSON line to stdout before responses")
	tools := flag.Int("tools", 1, "number of fake tools to return")
	flag.Parse()

	if *polluteStdout {
		fmt.Println("this is not json-rpc")
	}
	fmt.Fprintln(os.Stderr, "fake MCP server started")

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		id, hasID := request["id"]
		method, _ := request["method"].(string)
		if !hasID {
			continue
		}
		switch method {
		case "initialize":
			if *delay > 0 {
				time.Sleep(*delay)
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"serverInfo":      map[string]any{"name": "fake-mcp-server", "version": "test"},
					"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				},
			})
		case "tools/list":
			items := make([]map[string]any, *tools)
			for i := range items {
				items[i] = map[string]any{
					"name":        fmt.Sprintf("fake_%d", i+1),
					"description": "fake tool",
					"inputSchema": map[string]any{"type": "object"},
				}
			}
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  map[string]any{"tools": items},
			})
		default:
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}
}
