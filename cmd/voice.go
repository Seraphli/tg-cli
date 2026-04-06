package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
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

	// Step 2: Engine selection
	appCfgForEngine, _ := config.LoadAppConfig()
	currentEngine := appCfgForEngine.VoiceEngine
	if currentEngine == "" {
		currentEngine = "whisper"
	}
	fmt.Println("Select voice engine:")
	if currentEngine == "whisper" {
		fmt.Println("  1. whisper (whisper.cpp, local inference) [current]")
	} else {
		fmt.Println("  1. whisper (whisper.cpp, local inference)")
	}
	if currentEngine == "sensevoice" {
		fmt.Println("  2. sensevoice (sherpa-onnx SenseVoice, faster multilingual) [current]")
	} else {
		fmt.Println("  2. sensevoice (sherpa-onnx SenseVoice, faster multilingual)")
	}
	fmt.Print("Engine choice (1-2, default 1): ")
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	engineChoice := strings.TrimSpace(scanner.Text())
	if engineChoice == "2" {
		runVoiceSherpa(scanner, ffmpegPath)
		return
	}

	// Step 3: Detect or ask for whisper.cpp
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
		fmt.Println("whisper.cpp not found in PATH.")
		fmt.Println()
		if _, err := os.Stat("/etc/arch-release"); err == nil {
			fmt.Println("Install via AUR (GPU support):")
			fmt.Println("  yay -S whisper.cpp-cuda")
		} else {
			fmt.Println("Install from source: https://github.com/ggml-org/whisper.cpp")
		}
		fmt.Println()
		fmt.Print("Or enter path to whisper.cpp binary: ")
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

	// Step 4: Model selection
	appCfg, _ := config.LoadAppConfig()
	currentModelName := ""
	if appCfg.ModelPath != "" {
		base := filepath.Base(appCfg.ModelPath)
		for _, m := range helpers.WhisperModels {
			if m.Filename == base {
				currentModelName = m.Name
				break
			}
		}
	}
	fmt.Println("\nAvailable whisper models:")
	modelsDir := filepath.Join(config.GetConfigDir(), "models")
	home, _ := os.UserHomeDir()
	systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
	for i, m := range helpers.WhisperModels {
		status := "download required"
		if _, err := os.Stat(filepath.Join(modelsDir, m.Filename)); err == nil {
			status = "installed"
		} else if _, err := os.Stat(filepath.Join(systemModelsDir, m.Filename)); err == nil {
			status = "installed"
		}
		if m.Name == currentModelName {
			fmt.Printf("  %d. %-15s mem %s | WER %s (%s) [current]\n", i+1, m.Name, m.Mem, m.WER, status)
		} else {
			fmt.Printf("  %d. %-15s mem %s | WER %s (%s)\n", i+1, m.Name, m.Mem, m.WER, status)
		}
	}
	if currentModelName != "" {
		fmt.Printf("\nCurrent model: %s\n", currentModelName)
		fmt.Print("Select model (1-6, Enter to keep current): ")
	} else {
		fmt.Print("\nSelect model (1-6): ")
	}
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	choice := strings.TrimSpace(scanner.Text())
	var selected helpers.VoiceModelInfo
	if choice == "" && currentModelName != "" {
		for _, m := range helpers.WhisperModels {
			if m.Name == currentModelName {
				selected = m
				break
			}
		}
		fmt.Printf("Keeping current model: %s\n", selected.Name)
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
		case "6":
			idx = 5
		default:
			fmt.Fprintln(os.Stderr, "Invalid selection")
			os.Exit(1)
		}
		selected = helpers.WhisperModels[idx]
	}

	// Download model or use existing
	localModelPath := filepath.Join(modelsDir, selected.Filename)
	systemModelPath := filepath.Join(systemModelsDir, selected.Filename)
	var modelPath string

	if _, err := os.Stat(localModelPath); err == nil {
		modelPath = localModelPath
		fmt.Printf("\nModel already exists at %s\n", modelPath)
	} else if _, err := os.Stat(systemModelPath); err == nil {
		modelPath = systemModelPath
		fmt.Printf("\nModel already exists at %s\n", modelPath)
	} else {
		modelPath = systemModelPath
		modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.Filename)
		if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create models directory: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nDownloading model from %s...\n", modelURL)
		if err := helpers.DownloadFile(modelPath, modelURL); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to download model: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Model downloaded to %s\n", modelPath)
	}

	// Step 5: Language selection
	fmt.Print("\nEnter language code (e.g., en, zh, ja) or press Enter for auto-detect: ")
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	language := strings.TrimSpace(scanner.Text())
	if language == "auto" || language == "" {
		language = ""
	}

	// Test engine before saving
	fmt.Println("\nTesting whisper engine...")
	testWav := filepath.Join(os.TempDir(), "tg-cli-voice-test.wav")
	testCmd := exec.Command(ffmpegPath, "-y", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "1", testWav)
	if out, err := testCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate test audio: %v\n%s\n", err, out)
		fmt.Fprintln(os.Stderr, "Config not saved.")
		os.Exit(1)
	}
	defer os.Remove(testWav)
	testOutBase := filepath.Join(os.TempDir(), fmt.Sprintf("tg-cli-whisper-test-%d", time.Now().UnixNano()))
	testArgs := []string{"-m", modelPath, "-f", testWav, "-otxt", "-of", testOutBase, "-nt"}
	testLang := language
	if testLang == "" {
		testLang = "auto"
	}
	testArgs = append(testArgs, "-l", testLang)
	wTest := exec.Command(whisperPath, testArgs...)
	if out, err := wTest.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "\nEngine test FAILED: %v\n%s\n", err, out)
		fmt.Fprintln(os.Stderr, "Config not saved. Please check your whisper.cpp installation.")
		os.Exit(1)
	}
	os.Remove(testOutBase + ".txt")
	fmt.Println("Engine test passed!")

	// Step 6: Save config (preserve existing config fields)
	cfg, _ := config.LoadAppConfig()
	cfg.WhisperPath = whisperPath
	cfg.ModelPath = modelPath
	cfg.Language = language
	cfg.FFmpegPath = ffmpegPath
	cfg.VoiceEngine = "whisper"
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


// runVoiceSherpa handles setup for the sherpa-onnx SenseVoice engine.
func runVoiceSherpa(scanner *bufio.Scanner, ffmpegPath string) {
	// Step 1: Ensure sherpa-onnx-offline binary
	sherpaPath, err := helpers.EnsureSherpaOnnx()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("sherpa-onnx: %s\n\n", sherpaPath)

	// Step 2: Ensure SenseVoice model
	modelPath, err := helpers.EnsureSenseVoiceModel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("SenseVoice model: %s\n\n", modelPath)

	appCfg, _ := config.LoadAppConfig()

	// Step 3: Language selection
	fmt.Print("Enter language code (e.g., en, zh, ja) or press Enter for auto-detect: ")
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Failed to read input")
		os.Exit(1)
	}
	language := strings.TrimSpace(scanner.Text())
	if language == "auto" || language == "" {
		language = ""
	}

	// Test engine before saving
	fmt.Println("\nTesting SenseVoice engine...")
	testWav := filepath.Join(os.TempDir(), "tg-cli-voice-test.wav")
	testCmd := exec.Command(ffmpegPath, "-y", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono", "-t", "1", testWav)
	if tOut, tErr := testCmd.CombinedOutput(); tErr != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate test audio: %v\n%s\n", tErr, tOut)
		fmt.Fprintln(os.Stderr, "Config not saved.")
		os.Exit(1)
	}
	defer os.Remove(testWav)
	tokensPath := filepath.Join(filepath.Dir(modelPath), "tokens.txt")
	testArgs := []string{"--sense-voice-model=" + modelPath, "--tokens=" + tokensPath}
	if language != "" {
		testArgs = append(testArgs, "--sense-voice-language="+language)
	}
	testArgs = append(testArgs, testWav)
	sTest := exec.Command(sherpaPath, testArgs...)
	if tOut, tErr := sTest.CombinedOutput(); tErr != nil {
		fmt.Fprintf(os.Stderr, "\nEngine test FAILED: %v\n%s\n", tErr, tOut)
		fmt.Fprintln(os.Stderr, "Config not saved. Please check your sherpa-onnx installation.")
		os.Exit(1)
	}
	fmt.Println("Engine test passed!")

	// Step 4: Save config (preserve existing whisper config fields)
	cfg := appCfg
	cfg.FFmpegPath = ffmpegPath
	cfg.VoiceEngine = "sensevoice"
	cfg.SherpaOnnxPath = sherpaPath
	cfg.SenseVoiceModelPath = modelPath
	cfg.Language = language
	if err := config.SaveAppConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nSenseVoice setup complete!")
	fmt.Printf("  sherpa-onnx: %s\n", sherpaPath)
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

