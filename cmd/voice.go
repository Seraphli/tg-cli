package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

type modelInfo struct {
	name     string
	filename string
}

var whisperModels = []modelInfo{
	{"tiny", "ggml-tiny.bin"},
	{"base", "ggml-base.bin"},
	{"small", "ggml-small.bin"},
	{"medium", "ggml-medium.bin"},
	{"large", "ggml-large-v3-turbo.bin"},
}

var VoiceCmd = &cobra.Command{
	Use:   "voice",
	Short: "Set up voice transcription (ffmpeg + whisper.cpp)",
	Run:   runVoice,
}

func runVoice(cmd *cobra.Command, args []string) {
	scanner := bufio.NewScanner(os.Stdin)

	// Step 1: Detect ffmpeg
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		fmt.Println("ffmpeg not found. Please install ffmpeg first:")
		fmt.Println()
		switch runtime.GOOS {
		case "linux":
			if _, err := os.Stat("/etc/arch-release"); err == nil {
				fmt.Println("  sudo pacman -S ffmpeg")
			} else {
				fmt.Println("  # Arch/Manjaro:")
				fmt.Println("  sudo pacman -S ffmpeg")
				fmt.Println()
				fmt.Println("  # Ubuntu/Debian:")
				fmt.Println("  sudo apt install ffmpeg")
			}
		case "darwin":
			fmt.Println("  brew install ffmpeg")
		default:
			fmt.Println("  Visit https://ffmpeg.org/download.html")
		}
		os.Exit(1)
	}
	fmt.Printf("ffmpeg found: %s\n\n", ffmpegPath)

	// Step 2: Detect or ask for whisper.cpp
	var whisperPath string
	for _, name := range []string{"whisper-cli", "whisper-cpp", "whisper"} {
		if p, err := exec.LookPath(name); err == nil {
			whisperPath = p
			break
		}
	}
	if whisperPath != "" {
		fmt.Printf("whisper.cpp found: %s\n\n", whisperPath)
	} else {
		fmt.Print("whisper.cpp not found in PATH. Enter path to whisper.cpp binary: ")
		if !scanner.Scan() {
			fmt.Fprintln(os.Stderr, "Failed to read input")
			os.Exit(1)
		}
		whisperPath = strings.TrimSpace(scanner.Text())
		if whisperPath == "" {
			fmt.Fprintln(os.Stderr, "No path provided")
			os.Exit(1)
		}
		whisperPath = expandHome(whisperPath)
		if _, err := os.Stat(whisperPath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "File not found: %s\n", whisperPath)
			os.Exit(1)
		}
	}

	// Step 3: Model selection
	appCfg, _ := config.LoadAppConfig()
	currentModelName := ""
	if appCfg.ModelPath != "" {
		base := filepath.Base(appCfg.ModelPath)
		for _, m := range whisperModels {
			if m.filename == base {
				currentModelName = m.name
				break
			}
		}
	}
	fmt.Println("\nAvailable whisper models:")
	modelsDir := filepath.Join(config.GetConfigDir(), "models")
	home, _ := os.UserHomeDir()
	systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
	for i, m := range whisperModels {
		status := "download required"
		if _, err := os.Stat(filepath.Join(modelsDir, m.filename)); err == nil {
			status = "installed"
		} else if _, err := os.Stat(filepath.Join(systemModelsDir, m.filename)); err == nil {
			status = "installed"
		}
		if m.name == currentModelName {
			fmt.Printf("  %d. %s (%s) [current]\n", i+1, m.name, status)
		} else {
			fmt.Printf("  %d. %s (%s)\n", i+1, m.name, status)
		}
	}
	if currentModelName != "" {
		fmt.Printf("\nCurrent model: %s\n", currentModelName)
		fmt.Print("Select model (1-5, Enter to keep current): ")
	} else {
		fmt.Print("\nSelect model (1-5): ")
	}
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	choice := strings.TrimSpace(scanner.Text())
	var selected modelInfo
	if choice == "" && currentModelName != "" {
		for _, m := range whisperModels {
			if m.name == currentModelName {
				selected = m
				break
			}
		}
		fmt.Printf("Keeping current model: %s\n", selected.name)
	} else {
		idx := -1
		switch choice {
		case "1":
			idx = 0
		case "2":
			idx = 1
		case "3":
			idx = 2
		case "4":
			idx = 3
		case "5":
			idx = 4
		default:
			fmt.Fprintln(os.Stderr, "Invalid selection")
			os.Exit(1)
		}
		selected = whisperModels[idx]
	}

	// Download model or use existing
	localModelPath := filepath.Join(modelsDir, selected.filename)
	systemModelPath := filepath.Join(systemModelsDir, selected.filename)
	var modelPath string

	if _, err := os.Stat(localModelPath); err == nil {
		modelPath = localModelPath
		fmt.Printf("\nModel already exists at %s\n", modelPath)
	} else if _, err := os.Stat(systemModelPath); err == nil {
		modelPath = systemModelPath
		fmt.Printf("\nModel already exists at %s\n", modelPath)
	} else {
		modelPath = systemModelPath
		modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.filename)
		if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create models directory: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nDownloading model from %s...\n", modelURL)
		if err := downloadFile(modelPath, modelURL); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to download model: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Model downloaded to %s\n", modelPath)
	}

	// Step 4: Language selection
	fmt.Print("\nEnter language code (e.g., en, zh, ja) or press Enter for auto-detect: ")
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	language := strings.TrimSpace(scanner.Text())
	if language == "auto" || language == "" {
		language = ""
	}

	// Step 5: Save config
	cfg := config.AppConfig{
		WhisperPath: whisperPath,
		ModelPath:   modelPath,
		Language:    language,
		FFmpegPath:  ffmpegPath,
	}
	if err := config.SaveAppConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nVoice transcription setup complete!")
	fmt.Printf("  Whisper: %s\n", whisperPath)
	fmt.Printf("  Model: %s\n", modelPath)
	fmt.Printf("  FFmpeg: %s\n", ffmpegPath)
	if language != "" {
		fmt.Printf("  Language: %s\n", language)
	} else {
		fmt.Println("  Language: auto-detect")
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()
	total := resp.ContentLength
	downloaded := int64(0)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			downloaded += int64(n)
			if total > 0 {
				percent := float64(downloaded) / float64(total) * 100
				fmt.Printf("\rProgress: %.1f%%", percent)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	fmt.Println()
	return nil
}
