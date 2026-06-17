package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// agentPythonBin is the path to the Python binary inside the dedicated venv.
// Set by initPythonEnv() at startup. Falls back to "python3" if venv init fails.
var agentPythonBin = "python3"

const agentVenvDir = "./agent_venv"

// initPythonEnv creates a dedicated venv and installs required packages.
// Called once at startup. Idempotent — skips creation if the venv already exists.
// Non-fatal: logs a warning and continues if setup fails; read_file tool will
// fail gracefully with a ScriptError message instead of crashing the server.
func initPythonEnv() {
	pythonBin := venvPythonBin()

	if _, err := os.Stat(pythonBin); err == nil {
		agentPythonBin = pythonBin
		fmt.Printf("[python] venv ready (existing): %s\n", pythonBin)
		return
	}

	fmt.Println("[python] creating agent venv...")

	if out, err := exec.Command("python3", "-m", "venv", agentVenvDir).CombinedOutput(); err != nil {
		fmt.Printf("[python] WARNING: venv create failed: %v\n%s\n", err, out)
		fmt.Println("[python] read_file tool will use system python3 (may lack packages)")
		return
	}

	pipBin := venvPipBin()
	if out, err := exec.Command(pipBin, "install", "--quiet", "--upgrade", "pip").CombinedOutput(); err != nil {
		fmt.Printf("[python] WARNING: pip upgrade failed: %v\n%s\n", err, out)
	}

	packages := []string{"openpyxl", "python-docx", "pdfminer.six"}
	args := append([]string{"install", "--quiet"}, packages...)
	if out, err := exec.Command(pipBin, args...).CombinedOutput(); err != nil {
		fmt.Printf("[python] WARNING: pip install failed: %v\n%s\n", err, out)
		fmt.Println("[python] read_file tool may not work correctly")
		return
	}

	agentPythonBin = pythonBin
	fmt.Printf("[python] venv ready (new): %s\n  packages: %v\n", pythonBin, packages)
}

func venvPythonBin() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(agentVenvDir, "Scripts", "python.exe")
	}
	return filepath.Join(agentVenvDir, "bin", "python3")
}

func venvPipBin() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(agentVenvDir, "Scripts", "pip.exe")
	}
	return filepath.Join(agentVenvDir, "bin", "pip")
}
