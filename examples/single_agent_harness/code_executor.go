package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	codeExecTimeout   = 90 * time.Second
	codeExecMaxOutput = 12_000

	codeExecImageDefault = "mas-agent-exec:latest"
)

var codeExecImage string

func initCodeExec() {
	codeExecImage = getEnv("CODE_EXEC_IMAGE", codeExecImageDefault)
}

// execExportPreamble is prepended to every Python execution.
// On exit (even sys.exit()) it reads all /tmp files and prints them
// as "__AGENT_EXPORT__:{json}" so the host can pick them up as artifacts.
// This mirrors the Python lab's _EXEC_EXPORT_PREAMBLE exactly.
const execExportPreamble = "import atexit as _axat,os as _axos,json as _axjson,sys as _axsys,base64 as _axb64\n" +
	"def _ax_xp(_oo=_axos,_jj=_axjson,_sy=_axsys,_b=_axb64):\n" +
	" try:\n" +
	"  _fs={}\n" +
	"  for _n in _oo.listdir('/tmp'):\n" +
	"   if not _oo.path.isfile('/tmp/'+_n):continue\n" +
	"   _raw=open('/tmp/'+_n,'rb').read()\n" +
	"   try:_fs[_n]=_raw.decode('utf-8');_fs[_n+'__bin__']='0'\n" +
	"   except:_fs[_n]=_b.b64encode(_raw).decode();_fs[_n+'__bin__']='1'\n" +
	"  if _fs:_sy.stdout.write('__AGENT_EXPORT__:'+_jj.dumps(_fs,ensure_ascii=False)+'\\n');_sy.stdout.flush()\n" +
	" except:pass\n" +
	"_axat.register(_ax_xp)\n" +
	"del _axat,_axos,_axjson,_axsys,_axb64,_ax_xp\n\n"

// executeCode runs code in a fresh Docker container and returns (output, hasError).
// Language: "python" | "bash".
// Streams lines to eventCh as tool_stream events.
// Files written to /tmp by the code are automatically exported as session artifacts.
func executeCode(ctx context.Context, language, code, sessionID string, eventCh chan<- SSEEvent) (string, bool) {
	// Check docker CLI is available
	if _, err := exec.LookPath("docker"); err != nil {
		return "Error: docker CLI not found in PATH. Install Docker Desktop and ensure it is running.", true
	}

	var interp string
	var fullCode string
	switch strings.ToLower(language) {
	case "python", "python3", "py", "":
		interp = "python3"
		fullCode = execExportPreamble + code
	case "bash", "sh", "shell":
		interp = "/bin/bash"
		fullCode = code
	default:
		return fmt.Sprintf("Error: unsupported language '%s'. Supported: python, bash.", language), true
	}

	// Write code to temp file — mounted into the container read-only.
	ext := ".py"
	if interp != "python3" {
		ext = ".sh"
	}
	tmp, err := os.CreateTemp("", "agent_code_*"+ext)
	if err != nil {
		return "Error creating temp file: " + err.Error(), true
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(fullCode); err != nil {
		tmp.Close()
		return "Error writing code: " + err.Error(), true
	}
	tmp.Close()

	// Build docker run command.
	// Security flags mirror the Python lab:
	//   -m 512m          — memory cap
	//   --cpus 1         — 1 CPU core
	//   --pids-limit 128 — prevent fork bomb
	//   --cap-drop ALL   — drop all Linux capabilities
	//   --security-opt no-new-privileges
	//   --network none   — no outbound network (agent uses web_search instead)
	// /tmp is a writable tmpfs so code can create output files.
	// /code is the user script, mounted read-only.
	dockerArgs := []string{
		"run", "--rm",
		"-m", "512m",
		"--cpus", "1",
		"--pids-limit", "128",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:exec,size=256m",
		"-v", tmp.Name() + ":/code" + ext + ":ro",
	}

	// Mount session upload directory if it has files.
	// Docker bind mounts require an absolute path — relative paths are treated as named volumes.
	if sessionID != "" {
		udir := sessionUploadDir(sessionID)
		if entries, _ := os.ReadDir(udir); len(entries) > 0 {
			absUdir, err := filepath.Abs(udir)
			if err != nil {
				absUdir = udir
			}
			dockerArgs = append(dockerArgs, "-v", absUdir+":/uploaded:ro")
			fmt.Printf("[code_exec] mounting /uploaded from %s (%d files)\n", absUdir, len(entries))
		}
	}

	dockerArgs = append(dockerArgs, codeExecImage, interp, "/code"+ext)

	runCtx, cancel := context.WithTimeout(ctx, codeExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "docker", dockerArgs...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "Error: " + err.Error(), true
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		// Image not found is the most common failure.
		if strings.Contains(startErr.Error(), "Unable to find image") ||
			strings.Contains(startErr.Error(), "No such image") {
			return fmt.Sprintf("Error: image '%s' not found.\nRun: .\\scripts\\build_agent_image.ps1", codeExecImage), true
		}
		return "Error starting container: " + startErr.Error(), true
	}

	// Stream stdout line-by-line.
	var outputLines []string
	var exportPayload string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "__AGENT_EXPORT__:") {
			exportPayload = strings.TrimPrefix(line, "__AGENT_EXPORT__:")
			continue // hide from output
		}
		outputLines = append(outputLines, line)
		emit(eventCh, "tool_stream", map[string]string{"name": "execute_python", "text": line + "\n"})
	}

	var hasError bool
	if waitErr := cmd.Wait(); waitErr != nil {
		hasError = true
		if runCtx.Err() == context.DeadlineExceeded {
			outputLines = append(outputLines, fmt.Sprintf("\n⏱ Timeout: execution exceeded %s and was killed.", codeExecTimeout))
		}
		// Non-zero exit — fall through, include stderr below.
	}

	// Attach stderr if non-empty.
	if s := stderrBuf.String(); s != "" {
		hasError = true
		outputLines = append(outputLines, "[stderr]")
		outputLines = append(outputLines, strings.TrimRight(s, "\n"))
	}

	// Present exported files as session artifacts.
	var exportedNames []string
	if exportPayload != "" {
		exportedNames = presentExportedFiles(sessionID, exportPayload, eventCh)
	}

	result := strings.Join(outputLines, "\n")
	if strings.TrimSpace(result) == "" {
		result = "(no output)"
	}
	if len(result) > codeExecMaxOutput {
		result = result[:codeExecMaxOutput] + "\n… [truncated]"
	}
	if len(exportedNames) > 0 {
		result += "\n📎 Exported: " + strings.Join(exportedNames, ", ")
	}
	return result, hasError
}

// presentExportedFiles parses the __AGENT_EXPORT__ JSON payload, presents each file
// as a session artifact, and returns the list of presented filenames.
func presentExportedFiles(sessionID, jsonPayload string, eventCh chan<- SSEEvent) []string {
	var files map[string]string
	if err := json.Unmarshal([]byte(jsonPayload), &files); err != nil {
		fmt.Printf("[code_exec] export parse error: %v\n", err)
		return nil
	}
	presented := map[string]bool{}
	var names []string
	for key, val := range files {
		if strings.HasSuffix(key, "__bin__") {
			continue
		}
		if presented[key] {
			continue
		}
		mime := guessMime(key)
		if mime == "application/octet-stream" {
			continue
		}
		presented[key] = true

		isBin := files[key+"__bin__"] == "1"
		var raw []byte
		if isBin {
			var decErr error
			raw, decErr = base64.StdEncoding.DecodeString(val)
			if decErr != nil {
				fmt.Printf("[code_exec] base64 decode error for %s: %v\n", key, decErr)
				continue
			}
		} else {
			raw = []byte(val)
		}

		fmt.Printf("[code_exec] presenting exported file: %s (%dB)\n", key, len(raw))
		art := putArtifact(sessionID, key, raw, mime)
		emitFilePresent(eventCh, art)
		names = append(names, key)
	}
	return names
}

// sessionUploadDir returns the host path where session file uploads are stored.
// These are mounted read-only at /uploaded inside the Docker container.
func sessionUploadDir(sessionID string) string {
	base := getEnv("UPLOAD_DIR", "tmp/uploads")
	return base + "/" + sessionID
}
