package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const jsonPlaceholderBase = "https://jsonplaceholder.typicode.com"

// ToolDef describes a function tool for the DeepSeek function-calling API.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Fn          func(args map[string]any) string
}

// JSONPlaceholderTools is the full tool set exposed to the json_agent.
var JSONPlaceholderTools = []ToolDef{
	{
		Name:        "list_users",
		Description: "List all 10 users from JSONPlaceholder API.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Fn:          toolListUsers,
	},
	{
		Name:        "get_user",
		Description: "Get full profile of a user by ID (1–10).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID 1-10"}},"required":["user_id"]}`),
		Fn:          toolGetUser,
	},
	{
		Name:        "get_posts",
		Description: "Get posts written by a user (by user_id).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID"}},"required":["user_id"]}`),
		Fn:          toolGetPosts,
	},
	{
		Name:        "get_todos",
		Description: "Get todo list of a user (by user_id) with completion status.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"user_id":{"type":"integer","description":"User ID"}},"required":["user_id"]}`),
		Fn:          toolGetTodos,
	},
	{
		Name:        "get_comments",
		Description: "Get comments on a specific post (by post_id).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"post_id":{"type":"integer","description":"Post ID"}},"required":["post_id"]}`),
		Fn:          toolGetComments,
	},
}

func jpGet(path string) ([]byte, error) {
	resp, err := http.Get(jsonPlaceholderBase + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func intArg(args map[string]any, key string) int {
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func toolListUsers(_ map[string]any) string {
	data, err := jpGet("/users")
	if err != nil {
		return "Error fetching users: " + err.Error()
	}
	var users []map[string]any
	if json.Unmarshal(data, &users) != nil {
		return "Parse error"
	}
	var lines []string
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("#%v: %v (@%v) — %v",
			u["id"], u["name"], u["username"], u["email"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetUser(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/users/%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var u map[string]any
	if json.Unmarshal(data, &u) != nil || u["id"] == nil {
		return fmt.Sprintf("User %d not found.", id)
	}
	addr, _ := u["address"].(map[string]any)
	city := ""
	if addr != nil {
		city, _ = addr["city"].(string)
	}
	comp, _ := u["company"].(map[string]any)
	compName := ""
	if comp != nil {
		compName, _ = comp["name"].(string)
	}
	return fmt.Sprintf("#%v: %v (@%v)\nEmail: %v | Phone: %v | Website: %v\nCompany: %v | City: %v",
		u["id"], u["name"], u["username"], u["email"], u["phone"], u["website"], compName, city)
}

func toolGetPosts(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/posts?userId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var posts []map[string]any
	if json.Unmarshal(data, &posts) != nil {
		return "Parse error"
	}
	if len(posts) == 0 {
		return fmt.Sprintf("User %d has no posts.", id)
	}
	lines := []string{fmt.Sprintf("%d posts by user #%d:", len(posts), id)}
	for _, p := range posts {
		lines = append(lines, fmt.Sprintf("  [%v] %v", p["id"], p["title"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetTodos(args map[string]any) string {
	id := intArg(args, "user_id")
	data, err := jpGet(fmt.Sprintf("/todos?userId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var todos []map[string]any
	if json.Unmarshal(data, &todos) != nil {
		return "Parse error"
	}
	if len(todos) == 0 {
		return fmt.Sprintf("User %d has no todos.", id)
	}
	done := 0
	for _, t := range todos {
		if c, _ := t["completed"].(bool); c {
			done++
		}
	}
	lines := []string{fmt.Sprintf("Todos of user #%d: %d/%d completed", id, done, len(todos))}
	for _, t := range todos {
		mark := "○"
		if c, _ := t["completed"].(bool); c {
			mark = "✓"
		}
		lines = append(lines, fmt.Sprintf("  %s [%v] %v", mark, t["id"], t["title"]))
	}
	return strings.Join(lines, "\n")
}

func toolGetComments(args map[string]any) string {
	id := intArg(args, "post_id")
	data, err := jpGet(fmt.Sprintf("/comments?postId=%d", id))
	if err != nil {
		return "Error: " + err.Error()
	}
	var comments []map[string]any
	if json.Unmarshal(data, &comments) != nil {
		return "Parse error"
	}
	if len(comments) == 0 {
		return fmt.Sprintf("Post %d has no comments.", id)
	}
	lines := []string{fmt.Sprintf("%d comments on post #%d:", len(comments), id)}
	for _, c := range comments {
		body, _ := c["body"].(string)
		if len(body) > 120 {
			body = body[:120] + "..."
		}
		lines = append(lines, fmt.Sprintf("  [%v] %v (%v): %v", c["id"], c["name"], c["email"], body))
	}
	return strings.Join(lines, "\n")
}
