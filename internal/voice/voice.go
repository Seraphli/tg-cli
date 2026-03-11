package voice

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
)

var transcribeMu sync.Mutex

// transcribeWhisper converts an OGG voice file to text using ffmpeg + whisper.cpp.
func transcribeWhisper(oggPath string) (string, error) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.WhisperPath == "" || cfg.ModelPath == "" {
		return "", fmt.Errorf("whisper not configured, run 'tg-cli voice' to set up")
	}
	// Convert OGG to WAV (16kHz mono)
	wavPath := oggPath + ".wav"
	defer os.Remove(wavPath)
	ffCmd := exec.Command(cfg.FFmpegPath, "-y", "-i", oggPath, "-ar", "16000", "-ac", "1", wavPath)
	if out, err := ffCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w\n%s", err, out)
	}
	// Run whisper.cpp with a unique output path to avoid collisions
	outBase := filepath.Join(os.TempDir(), fmt.Sprintf("tg-cli-whisper-%d", time.Now().UnixNano()))
	args := []string{"-m", cfg.ModelPath, "-f", wavPath, "-otxt", "-of", outBase, "-nt"}
	lang := cfg.Language
	if lang == "" {
		lang = "auto"
	}
	args = append(args, "-l", lang)
	prompt := cfg.WhisperPrompt
	if prompt == "" {
		prompt = "Hello, how are you? 你好，请问有什么需要帮助的？"
	}
	args = append(args, "--prompt", prompt)
	wCmd := exec.Command(cfg.WhisperPath, args...)
	if out, err := wCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("whisper failed: %w\n%s", err, out)
	}
	txtPath := outBase + ".txt"
	defer os.Remove(txtPath)
	data, err := os.ReadFile(txtPath)
	if err != nil {
		return "", fmt.Errorf("failed to read transcription: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// transcribeSherpaOnnx converts an OGG voice file to text using sherpa-onnx-offline + SenseVoice model.
func transcribeSherpaOnnx(oggPath string) (string, error) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.SherpaOnnxPath == "" {
		return "", fmt.Errorf("sherpa-onnx not configured, run 'tg-cli voice' to set up")
	}
	if cfg.SenseVoiceModelPath == "" {
		return "", fmt.Errorf("SenseVoice model not configured, run 'tg-cli voice' to set up")
	}
	// Convert OGG to WAV (16kHz mono)
	wavPath := oggPath + ".wav"
	defer os.Remove(wavPath)
	ffCmd := exec.Command(cfg.FFmpegPath, "-y", "-i", oggPath, "-ar", "16000", "-ac", "1", wavPath)
	if out, err := ffCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w\n%s", err, out)
	}
	// Run sherpa-onnx-offline with SenseVoice model
	tokensPath := filepath.Join(filepath.Dir(cfg.SenseVoiceModelPath), "tokens.txt")
	args := []string{
		"--sense-voice-model=" + cfg.SenseVoiceModelPath,
		"--tokens=" + tokensPath,
		"--sense-voice-use-itn=true",
	}
	lang := cfg.Language
	if lang != "" {
		args = append(args, "--sense-voice-language="+lang)
	}
	args = append(args, wavPath)
	sCmd := exec.Command(cfg.SherpaOnnxPath, args...)
	out, err := sCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sherpa-onnx failed: %w\n%s", err, out)
	}
	// SenseVoice outputs JSON with "text" field
	result := strings.TrimSpace(string(out))
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			var parsed struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(line), &parsed); err == nil && parsed.Text != "" {
				return parsed.Text, nil
			}
		}
	}
	return "", fmt.Errorf("no transcription found in sherpa-onnx output")
}

// retainVoiceFile copies a voice file to ~/.tg-cli/voice-cache/, keeping only the last N files.
func retainVoiceFile(oggPath string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cacheDir := filepath.Join(home, ".tg-cli", "voice-cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return
	}
	dst := filepath.Join(cacheDir, fmt.Sprintf("voice-%d.ogg", time.Now().UnixNano()))
	data, err := os.ReadFile(oggPath)
	if err != nil {
		return
	}
	os.WriteFile(dst, data, 0644)
	retainCount := 5
	if cfg, err := config.LoadAppConfig(); err == nil && cfg.VoiceRetainCount > 0 {
		retainCount = cfg.VoiceRetainCount
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, filepath.Join(cacheDir, e.Name()))
		}
	}
	sort.Strings(files)
	for len(files) > retainCount {
		os.Remove(files[0])
		files = files[1:]
	}
}

// Transcribe converts an OGG voice file to text using the configured engine.
// Returns (text, engineName, error). A mutex ensures concurrent voice messages are processed serially.
func Transcribe(oggPath string) (string, string, error) {
	transcribeMu.Lock()
	defer transcribeMu.Unlock()
	retainVoiceFile(oggPath)
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to load config: %w", err)
	}
	switch cfg.VoiceEngine {
	case "sensevoice":
		text, err := transcribeSherpaOnnx(oggPath)
		return text, "sensevoice", err
	default:
		text, err := transcribeWhisper(oggPath)
		return text, "whisper", err
	}
}
