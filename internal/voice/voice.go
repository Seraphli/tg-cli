package voice

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
)

// Transcribe converts an OGG voice file to text using ffmpeg + whisper.cpp.
func Transcribe(oggPath string) (string, error) {
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
	// Run whisper.cpp
	outBase := filepath.Join(os.TempDir(), "tg-cli-whisper")
	args := []string{"-m", cfg.ModelPath, "-f", wavPath, "-otxt", "-of", outBase, "-nt"}
	if cfg.Language != "" {
		args = append(args, "-l", cfg.Language)
	}
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
