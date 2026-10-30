package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/kernel"
)

// The demo's one tool: a shell. It is RiskConfirm, so the agent parks the run
// and the UI must approve every call before anything runs. This is a local
// developer demo with no sandbox: approval is the only guard, and the command
// runs with the demo user's privileges.
const shellTimeout = 60 * time.Second

type shellInput struct {
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
}

type shellOutput struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// The schema is hand-written rather than derived so the fields carry
// descriptions for the model; the derived form has no description tag.
var shellSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command line, run with sh -c. Prefer one command per call."
    },
    "workdir": {
      "type": "string",
      "description": "Working directory for the command. Omit to use the demo's directory."
    }
  },
  "required": ["command"]
}`)

func shellTool() agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name: "shell",
		Description: "Run a shell command and return its stdout, stderr and exit code. " +
			"Every call pauses for the user's permission before it runs.",
		Schema:    shellSchema,
		Risk:      agent.RiskConfirm,
		Timeout:   shellTimeout,
		MaxOutput: 64 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in shellInput) (shellOutput, error) {
		out := shellOutput{Command: in.Command}
		if in.Command == "" {
			return out, kernel.NewError(kernel.CodeInvalid, "command must not be empty")
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
		if in.Workdir != "" {
			cmd.Dir = in.Workdir
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		out.Stdout = stdout.String()
		out.Stderr = stderr.String()
		if err != nil {
			var exit *exec.ExitError
			switch {
			case errors.As(err, &exit):
				out.ExitCode = exit.ExitCode()
			case ctx.Err() != nil:
				return out, ctx.Err()
			default:
				return out, err
			}
		}
		return out, nil
	})
}
