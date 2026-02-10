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
	fmt.Println("\nAvailable whisper models:")
	models := []string{"tiny", "base", "small", "medium", "large"}
	modelsDir := filepath.Join(config.GetConfigDir(), "models")
	home, _ := os.UserHomeDir()
	systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
	for i, model := range models {
		modelName := fmt.Sprintf("ggml-%s.bin", model)
		if _, err := os.Stat(filepath.Join(modelsDir, modelName)); err == nil {
			fmt.Printf("  %d. %s (installed)\n", i+1, model)
		} else if _, err := os.Stat(filepath.Join(systemModelsDir, modelName)); err == nil {
			fmt.Printf("  %d. %s (installed: %s)\n", i+1, model, filepath.Join(systemModelsDir, modelName))
		} else {
			fmt.Printf("  %d. %s (download required)\n", i+1, model)
		}
	}
	fmt.Print("\nSelect model (1-5): ")
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	choice := strings.TrimSpace(scanner.Text())
	var selectedModel string
	switch choice {
	case "1":
		selectedModel = "tiny"
	case "2":
		selectedModel = "base"
	case "3":
		selectedModel = "small"
	case "4":
		selectedModel = "medium"
	case "5":
		selectedModel = "large"
	default:
		fmt.Fprintln(os.Stderr, "Invalid selection")
		os.Exit(1)
	}

	// Download model or use existing
	modelName := fmt.Sprintf("ggml-%s.bin", selectedModel)
	modelPath := filepath.Join(modelsDir, modelName)
	systemModelPath := filepath.Join(systemModelsDir, modelName)

	if _, err := os.Stat(modelPath); err == nil {
		fmt.Printf("\nModel already exists at %s\n", modelPath)
	} else if _, err := os.Stat(systemModelPath); err == nil {
		modelPath = systemModelPath
		fmt.Printf("\nUsing system model at %s\n", modelPath)
	} else {
		modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-%s.bin", selectedModel)
		if err := os.MkdirAll(modelsDir, 0755); err != nil {
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
