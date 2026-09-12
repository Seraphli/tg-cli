package voice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
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

// apiHTTPClient is the HTTP client used for the "api" engine. A package-level var
// lets tests inject a client with a short timeout.
var apiHTTPClient = &http.Client{Timeout: 5 * time.Minute}

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
	args = append(args, "--no-speech-thold", "0.8")
	args = append(args, "--entropy-thold", "2.0")
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
	return stripHallucinations(strings.TrimSpace(string(data))), nil
}

// stripHallucinations removes common whisper hallucination phrases from the end of transcription.
func stripHallucinations(text string) string {
	hallucinations := []string{
		"谢谢收听", "谢谢观看", "谢谢大家", "感谢收听", "感谢观看",
		"请订阅", "别忘了点赞", "点赞关注", "欢迎订阅",
		"Thank you.", "Thank you for watching.", "Thanks for watching.",
		"Thank you for listening.", "Thanks for listening.",
		"Please subscribe.", "Like and subscribe.",
		"Subtitles by", "字幕", "配音",
	}
	changed := true
	for changed {
		changed = false
		trimmed := strings.TrimSpace(text)
		for _, h := range hallucinations {
			if strings.HasSuffix(trimmed, h) {
				trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(h)])
				changed = true
			}
		}
		text = trimmed
	}
	return text
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

// apiTranscriptionResponse is the OpenAI-compatible JSON response shape.
type apiTranscriptionResponse struct {
	Text string `json:"text"`
}

// transcribeAPI converts a voice file to text using a generic OpenAI-compatible
// speech-to-text API. The file is uploaded as-is (no ffmpeg conversion).
func transcribeAPI(oggPath string) (string, error) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	if cfg.VoiceAPIBaseURL == "" {
		return "", fmt.Errorf("voiceApiBaseUrl not configured, run 'tg-cli voice' to set up")
	}
	if cfg.VoiceAPIModel == "" {
		return "", fmt.Errorf("voiceApiModel not configured, run 'tg-cli voice' to set up")
	}
	data, err := os.ReadFile(oggPath)
	if err != nil {
		return "", fmt.Errorf("failed to read voice file: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(oggPath))
	if err != nil {
		return "", fmt.Errorf("failed to build multipart form: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("failed to write file part: %w", err)
	}
	if err := writer.WriteField("model", cfg.VoiceAPIModel); err != nil {
		return "", fmt.Errorf("failed to write model field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close multipart form: %w", err)
	}
	url := strings.TrimRight(cfg.VoiceAPIBaseURL, "/") + "/v1/audio/transcriptions"
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.VoiceAPIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("api request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read api response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return "", fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, snippet)
	}
	var parsed apiTranscriptionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse api response: %w", err)
	}
	return parsed.Text, nil
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

// TranscribeWithEngine converts a voice file to text using the given engine.
// Returns (text, engineName, error). A mutex ensures concurrent voice messages are processed serially.
func TranscribeWithEngine(oggPath, engine string) (string, string, error) {
	transcribeMu.Lock()
	defer transcribeMu.Unlock()
	retainVoiceFile(oggPath)
	switch engine {
	case "sensevoice":
		text, err := transcribeSherpaOnnx(oggPath)
		return text, "sensevoice", err
	case "api":
		text, err := transcribeAPI(oggPath)
		return text, "api", err
	default:
		text, err := transcribeWhisper(oggPath)
		return text, "whisper", err
	}
}

// Transcribe converts an OGG voice file to text using the configured engine.
// Returns (text, engineName, error).
func Transcribe(oggPath string) (string, string, error) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to load config: %w", err)
	}
	return TranscribeWithEngine(oggPath, cfg.VoiceEngine)
}
