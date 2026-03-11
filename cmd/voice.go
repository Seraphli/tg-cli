package cmd

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

type modelInfo struct {
	name     string
	filename string
	mem      string // runtime memory (whisper.cpp README)
	wer      string // approximate multilingual WER (Whisper paper)
}

// Source: mem from whisper.cpp README, WER from OpenAI Whisper paper
// turbo mem estimated (~2.3 GB) — not in whisper.cpp table
var whisperModels = []modelInfo{
	{"tiny", "ggml-tiny.bin", "~273 MB", "~12%"},
	{"base", "ggml-base.bin", "~388 MB", "~10%"},
	{"small", "ggml-small.bin", "~852 MB", "~7%"},
	{"medium", "ggml-medium.bin", "~2.1 GB", "~5%"},
	{"large-v3-turbo", "ggml-large-v3-turbo.bin", "~2.3 GB", "~3.7%"},
	{"large-v3", "ggml-large-v3.bin", "~3.9 GB", "~3.5%"},
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
			fmt.Printf("  %d. %-15s mem %s | WER %s (%s) [current]\n", i+1, m.name, m.mem, m.wer, status)
		} else {
			fmt.Printf("  %d. %-15s mem %s | WER %s (%s)\n", i+1, m.name, m.mem, m.wer, status)
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
		case "6":
			idx = 5
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

// detectSherpaOnnxBinary searches for sherpa-onnx-offline binary in PATH and known locations.
func detectSherpaOnnxBinary() string {
	for _, name := range []string{"sherpa-onnx-offline", "sherpa-onnx"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	configDir := config.GetConfigDir()
	homeDir, _ := os.UserHomeDir()
	patterns := []string{
		filepath.Join(configDir, "sherpa-onnx", "*", "bin", "sherpa-onnx-offline"),
		filepath.Join(homeDir, "sherpa-onnx*", "bin", "sherpa-onnx-offline"),
		filepath.Join(homeDir, "Workspace", "Github", "*", "*", "sherpa-onnx*", "bin", "sherpa-onnx-offline"),
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		if len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// detectSenseVoiceModel searches for SenseVoice model in config and known locations.
func detectSenseVoiceModel() string {
	cfg, err := config.LoadAppConfig()
	if err == nil && cfg.SenseVoiceModelPath != "" {
		if _, err := os.Stat(cfg.SenseVoiceModelPath); err == nil {
			return cfg.SenseVoiceModelPath
		}
	}
	configDir := config.GetConfigDir()
	matches, _ := filepath.Glob(filepath.Join(configDir, "models", "sensevoice", "*", "model.int8.onnx"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func ensureSherpaOnnx() (string, error) {
	sherpaPath := detectSherpaOnnxBinary()
	if sherpaPath != "" {
		return sherpaPath, nil
	}
	configDir := config.GetConfigDir()
	sherpaOnnxVersion := "1.12.28"
	sherpaURL := fmt.Sprintf("https://github.com/k2-fsa/sherpa-onnx/releases/download/v%s/sherpa-onnx-v%s-linux-x64-gpu.tar.bz2", sherpaOnnxVersion, sherpaOnnxVersion)
	destDir := filepath.Join(configDir, "sherpa-onnx")
	topDir, err := downloadAndExtractTarBz2(sherpaURL, destDir)
	if err != nil {
		return "", fmt.Errorf("failed to download sherpa-onnx: %w", err)
	}
	sherpaPath = filepath.Join(topDir, "bin", "sherpa-onnx-offline")
	if _, err := os.Stat(sherpaPath); os.IsNotExist(err) {
		return "", fmt.Errorf("sherpa-onnx-offline binary not found in extracted archive at %s", sherpaPath)
	}
	return sherpaPath, nil
}

func ensureSenseVoiceModel() (string, error) {
	modelPath := detectSenseVoiceModel()
	if modelPath != "" {
		return modelPath, nil
	}
	configDir := config.GetConfigDir()
	modelsDir := filepath.Join(configDir, "models", "sensevoice")
	modelURL := "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17.tar.bz2"
	topDir, err := downloadAndExtractTarBz2(modelURL, modelsDir)
	if err != nil {
		return "", fmt.Errorf("failed to download SenseVoice model: %w", err)
	}
	modelPath = filepath.Join(topDir, "model.int8.onnx")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return "", fmt.Errorf("model file not found in extracted archive at %s", modelPath)
	}
	return modelPath, nil
}

func ensureWhisperModel() (string, error) {
	cfg, _ := config.LoadAppConfig()
	if cfg.ModelPath != "" {
		if _, err := os.Stat(cfg.ModelPath); err == nil {
			return cfg.ModelPath, nil
		}
	}
	modelsDir := filepath.Join(config.GetConfigDir(), "models")
	home, _ := os.UserHomeDir()
	systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
	for _, m := range whisperModels {
		if p := filepath.Join(modelsDir, m.filename); fileExists(p) {
			return p, nil
		}
		if p := filepath.Join(systemModelsDir, m.filename); fileExists(p) {
			return p, nil
		}
	}
	// Download default model (base)
	selected := whisperModels[1]
	modelPath := filepath.Join(systemModelsDir, selected.filename)
	modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.filename)
	if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create models directory: %w", err)
	}
	if err := downloadFile(modelPath, modelURL); err != nil {
		return "", fmt.Errorf("failed to download whisper model: %w", err)
	}
	return modelPath, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// currentWhisperModelName returns the name of the currently configured whisper model.
func currentWhisperModelName() string {
	cfg, _ := config.LoadAppConfig()
	if cfg.ModelPath == "" {
		return ""
	}
	base := filepath.Base(cfg.ModelPath)
	for _, m := range whisperModels {
		if m.filename == base {
			return m.name
		}
	}
	return ""
}

// runVoiceSherpa handles setup for the sherpa-onnx SenseVoice engine.
func runVoiceSherpa(scanner *bufio.Scanner, ffmpegPath string) {
	// Step 1: Ensure sherpa-onnx-offline binary
	sherpaPath, err := ensureSherpaOnnx()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("sherpa-onnx: %s\n\n", sherpaPath)

	// Step 2: Ensure SenseVoice model
	modelPath, err := ensureSenseVoiceModel()
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

// downloadAndExtractTarBz2 downloads a tar.bz2 file and extracts it to destDir.
// Returns the path of the top-level directory inside the archive.
func downloadAndExtractTarBz2(url, destDir string) (string, error) {
	tmpFile := filepath.Join(destDir, "download.tar.bz2")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}
	fmt.Printf("Downloading from %s...\n", url)
	if err := downloadFile(tmpFile, url); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tmpFile)

	f, err := os.Open(tmpFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	bzReader := bzip2.NewReader(f)
	tarReader := tar.NewReader(bzReader)

	var topDir string
	fmt.Println("Extracting...")
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read error: %w", err)
		}

		target := filepath.Join(destDir, header.Name)
		if topDir == "" {
			parts := strings.SplitN(header.Name, "/", 2)
			if len(parts) > 0 && parts[0] != "" {
				topDir = filepath.Join(destDir, parts[0])
			}
		}

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(header.Mode))
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return "", err
			}
			io.Copy(outFile, tarReader)
			outFile.Close()
		}
	}
	fmt.Println("Extraction complete.")
	return topDir, nil
}
