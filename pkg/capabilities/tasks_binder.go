package capabilities

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aliothmoon/mcplex/pkg/policy"
	"github.com/aliothmoon/mcplex/pkg/sandbox"
	"github.com/aliothmoon/mcplex/pkg/tasks"
)

// TasksBinder exposes `tasks.*` inside the TypeScript sandbox so scripts can orchestrate
// long-running background tasks and daemons alongside local file/search/git capabilities.
type TasksBinder struct {
	Manager   *tasks.Manager
	Policy    *policy.Engine
	SessionID string
}

func (b *TasksBinder) Bind(env *sandbox.ExecutionEnv) error {
	if b == nil || b.Manager == nil {
		return nil
	}

	spawnFn := func(ctx context.Context, args []any) (any, error) {
		if len(args) == 0 {
			return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "command is required")
		}
		var req tasks.StartRequest
		req.Cwd = env.Cwd
		req.SessionID = b.SessionID

		switch first := args[0].(type) {
		case string:
			req.Command = first
			if len(args) > 1 {
				if opts, ok := args[1].(map[string]any); ok {
					applyStartOpts(&req, opts, env.Cwd)
				}
			}
		case map[string]any:
			req.Command = firstString(first, "command", "cmd")
			applyStartOpts(&req, first, env.Cwd)
		default:
			return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "first argument must be a command string or options object")
		}

		res, err := b.Manager.Start(ctx, b.Policy, req)
		if err != nil {
			return nil, err
		}
		return structToMap(res), nil
	}

	fns := map[string]sandbox.AsyncHostFunc{
		"spawn": spawnFn,
		"start": spawnFn,
		"list": func(ctx context.Context, _ []any) (any, error) {
			res, err := b.Manager.Manage(ctx, tasks.ManageRequest{
				Action:    "list",
				SessionID: b.SessionID,
			})
			if err != nil {
				return nil, err
			}
			return structToSlice(res.Tasks), nil
		},
		"status": func(ctx context.Context, args []any) (any, error) {
			if len(args) == 0 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "taskId is required")
			}
			req := tasks.ManageRequest{
				Action:    "status",
				TaskID:    fmt.Sprint(args[0]),
				SessionID: b.SessionID,
			}
			if len(args) > 1 {
				if opts, ok := args[1].(map[string]any); ok {
					req.Tail = optInt(opts, "tail", 0)
				}
			}
			res, err := b.Manager.Manage(ctx, req)
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
		"logs": func(ctx context.Context, args []any) (any, error) {
			if len(args) == 0 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "taskId is required")
			}
			req := tasks.ManageRequest{
				Action:    "logs",
				TaskID:    fmt.Sprint(args[0]),
				SessionID: b.SessionID,
			}
			if len(args) > 1 {
				if opts, ok := args[1].(map[string]any); ok {
					req.Tail = optInt(opts, "tail", 0)
					if _, hasOff := opts["offset"]; hasOff {
						off := int64(optInt(opts, "offset", 0))
						req.Offset = &off
					}
				}
			}
			res, err := b.Manager.Manage(ctx, req)
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
		"wait": func(ctx context.Context, args []any) (any, error) {
			if len(args) == 0 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "taskId is required")
			}
			req := tasks.ManageRequest{
				Action:    "wait",
				TaskID:    fmt.Sprint(args[0]),
				SessionID: b.SessionID,
			}
			if len(args) > 1 {
				if opts, ok := args[1].(map[string]any); ok {
					req.WaitMs = optInt(opts, "waitMs", 0)
					req.Expect = firstString(opts, "expect")
					req.Tail = optInt(opts, "tail", 0)
				}
			}
			res, err := b.Manager.Manage(ctx, req)
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
		"input": func(ctx context.Context, args []any) (any, error) {
			if len(args) < 2 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "pass (taskId, text, opts?)")
			}
			req := tasks.ManageRequest{
				Action:    "input",
				TaskID:    fmt.Sprint(args[0]),
				Input:     fmt.Sprint(args[1]),
				SessionID: b.SessionID,
			}
			if len(args) > 2 {
				if opts, ok := args[2].(map[string]any); ok {
					if _, ok := opts["appendNewline"]; ok {
						v := optBool(opts, "appendNewline", true)
						req.AppendNewline = &v
					}
					req.CloseStdin = optBool(opts, "closeStdin", false)
				}
			}
			res, err := b.Manager.Manage(ctx, req)
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
		"kill": func(ctx context.Context, args []any) (any, error) {
			if len(args) == 0 {
				return nil, sandbox.NewHostError(sandbox.KindBadArgs, nil, "taskId is required")
			}
			res, err := b.Manager.Manage(ctx, tasks.ManageRequest{
				Action:    "kill",
				TaskID:    fmt.Sprint(args[0]),
				SessionID: b.SessionID,
			})
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
		"killAll": func(ctx context.Context, _ []any) (any, error) {
			res, err := b.Manager.Manage(ctx, tasks.ManageRequest{
				Action:    "kill_all",
				SessionID: b.SessionID,
			})
			if err != nil {
				return nil, err
			}
			return structToMap(res), nil
		},
	}

	return env.SetNamespace("tasks", fns, nil)
}

func applyStartOpts(req *tasks.StartRequest, opts map[string]any, defaultCwd string) {
	if c := firstString(opts, "cwd"); c != "" {
		req.Cwd = c
	} else if req.Cwd == "" {
		req.Cwd = defaultCwd
	}
	req.Shell = firstString(opts, "shell")
	req.Daemon = optBool(opts, "daemon", false)
	req.Name = firstString(opts, "name")
	if _, ok := opts["waitMs"]; ok {
		w := optInt(opts, "waitMs", 0)
		req.WaitMs = &w
	}
	if _, ok := opts["timeoutSec"]; ok {
		ts := optInt(opts, "timeoutSec", 0)
		req.TimeoutSec = &ts
	}
	if envMap, ok := opts["env"].(map[string]any); ok && len(envMap) > 0 {
		req.Env = make(map[string]string, len(envMap))
		for k, v := range envMap {
			if strings.TrimSpace(k) != "" && v != nil {
				req.Env[k] = fmt.Sprint(v)
			}
		}
	}
}

func structToMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func structToSlice(v any) []any {
	raw, err := json.Marshal(v)
	if err != nil {
		return []any{}
	}
	var out []any
	_ = json.Unmarshal(raw, &out)
	return out
}
