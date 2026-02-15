package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage systemd user service",
}

func init() {
	ServiceCmd.AddCommand(serviceInstallCmd)
	ServiceCmd.AddCommand(serviceUninstallCmd)
	ServiceCmd.AddCommand(serviceStartCmd)
	ServiceCmd.AddCommand(serviceStopCmd)
	ServiceCmd.AddCommand(serviceRestartCmd)
	ServiceCmd.AddCommand(serviceStatusCmd)
	ServiceCmd.AddCommand(serviceUpgradeCmd)
}

func serviceName() string {
	if config.ConfigDir == "" {
		return "tg-cli"
	}
	base := filepath.Base(config.ConfigDir)
	if base == ".tg-cli" {
		return "tg-cli"
	}
	return "tg-cli-" + base
}

// installBinPath returns the path where the service binary should be installed.
func installBinPath() string {
	return filepath.Join(config.GetConfigDir(), "bin", "tg-cli")
}

func unitFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", serviceName()+".service")
}

const unitTemplate = `[Unit]
Description=tg-cli Telegram Bot{{if ne .ConfigDir ""}} ({{.ConfigDir}}){{end}}
After=network-online.target

[Service]
Type=simple
ExecStart={{.ExecStart}}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// copyFile copies src to dst with the given permissions.
func copyFile(src, dst string) error {
	os.MkdirAll(filepath.Dir(dst), 0755)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	os.Chmod(tmp, 0755)
	return os.Rename(tmp, dst)
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install systemd user service",
	Run: func(cmd *cobra.Command, args []string) {
		exePath, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
			os.Exit(1)
		}
		exePath, _ = filepath.Abs(exePath)
		binPath := installBinPath()
		fmt.Printf("Copying binary to %s...\n", binPath)
		if err := copyFile(exePath, binPath); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to copy binary: %v\n", err)
			os.Exit(1)
		}
		execStart := binPath + " bot"
		if config.ConfigDir != "" {
			execStart = binPath + " --config-dir " + config.ConfigDir + " bot"
		}
		unitPath := unitFilePath()
		os.MkdirAll(filepath.Dir(unitPath), 0755)
		tmpl, _ := template.New("unit").Parse(unitTemplate)
		f, err := os.Create(unitPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create unit file: %v\n", err)
			os.Exit(1)
		}
		tmpl.Execute(f, map[string]string{
			"ExecStart": execStart,
			"ConfigDir": config.ConfigDir,
		})
		f.Close()
		systemctl("daemon-reload")
		systemctl("enable", serviceName())
		fmt.Printf("Service %s (v%s) installed at %s\n", serviceName(), Version, unitPath)
	},
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall systemd user service",
	Run: func(cmd *cobra.Command, args []string) {
		name := serviceName()
		systemctl("stop", name)
		systemctl("disable", name)
		os.Remove(unitFilePath())
		systemctl("daemon-reload")
		os.Remove(installBinPath())
		fmt.Printf("Service %s uninstalled\n", name)
	},
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start service",
	Run:   func(cmd *cobra.Command, args []string) { systemctl("start", serviceName()) },
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop service",
	Run:   func(cmd *cobra.Command, args []string) { systemctl("stop", serviceName()) },
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart service",
	Run:   func(cmd *cobra.Command, args []string) { systemctl("restart", serviceName()) },
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show service status",
	Run:   func(cmd *cobra.Command, args []string) { systemctl("status", serviceName()) },
}

var serviceUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Rebuild binary and restart service",
	Run: func(cmd *cobra.Command, args []string) {
		binPath := installBinPath()
		if _, err := os.Stat(unitFilePath()); err != nil {
			fmt.Fprintf(os.Stderr, "Service not installed: %v\n", err)
			os.Exit(1)
		}
		tmpBin := binPath + ".build"
		fmt.Println("Building...")
		buildCmd := exec.Command("go", "build", "-o", tmpBin)
		buildCmd.Stdout = os.Stdout
		buildCmd.Stderr = os.Stderr
		if err := buildCmd.Run(); err != nil {
			os.Remove(tmpBin)
			fmt.Fprintf(os.Stderr, "Build failed: %v\n", err)
			os.Exit(1)
		}
		// Read old version from installed binary
		oldVersion := "unknown"
		if oldVerBytes, err := exec.Command(binPath, "--version").Output(); err == nil {
			oldVersion = strings.TrimSpace(string(oldVerBytes))
			if parts := strings.Fields(oldVersion); len(parts) >= 3 {
				oldVersion = parts[len(parts)-1]
			}
		}
		// Read new version from freshly built binary
		newVersion := "unknown"
		if newVerBytes, err := exec.Command(tmpBin, "--version").Output(); err == nil {
			newVersion = strings.TrimSpace(string(newVerBytes))
			if parts := strings.Fields(newVersion); len(parts) >= 3 {
				newVersion = parts[len(parts)-1]
			}
		}
		fmt.Printf("Upgrading from v%s to v%s\n", oldVersion, newVersion)
		fmt.Println("Stopping service...")
		systemctl("stop", serviceName())
		fmt.Printf("Replacing %s...\n", binPath)
		if err := os.Rename(tmpBin, binPath); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to replace binary: %v\n", err)
			os.Exit(1)
		}
		os.Chmod(binPath, 0755)
		fmt.Println("Starting service...")
		systemctl("start", serviceName())
		fmt.Printf("Upgrade complete (v%s)\n", newVersion)
	},
}

func systemctl(args ...string) {
	allArgs := append([]string{"--user"}, args...)
	c := exec.Command("systemctl", allArgs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Run()
}
