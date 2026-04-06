package helpers

import (
	"archive/tar"
	"compress/bzip2"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
)

// VoiceModelInfo describes a whisper.cpp model.
type VoiceModelInfo struct {
	Name     string
	Filename string
	Mem      string // runtime memory
	WER      string // approximate multilingual WER
}

// WhisperModels lists available whisper.cpp models.
var WhisperModels = []VoiceModelInfo{
	{"tiny", "ggml-tiny.bin", "~273 MB", "~12%"},
	{"base", "ggml-base.bin", "~388 MB", "~10%"},
	{"small", "ggml-small.bin", "~852 MB", "~7%"},
	{"medium", "ggml-medium.bin", "~2.1 GB", "~5%"},
	{"large-v3-turbo", "ggml-large-v3-turbo.bin", "~2.3 GB", "~3.7%"},
	{"large-v3", "ggml-large-v3.bin", "~3.9 GB", "~3.5%"},
}

// FileExists checks if a file exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CurrentWhisperModelName returns the name of the currently configured whisper model.
func CurrentWhisperModelName() string {
	cfg, _ := config.LoadAppConfig()
	if cfg.ModelPath == "" {
		return ""
	}
	base := filepath.Base(cfg.ModelPath)
	for _, m := range WhisperModels {
		if m.Filename == base {
			return m.Name
		}
	}
	return ""
}

// DetectSherpaOnnxBinary searches for sherpa-onnx binary in common locations.
func DetectSherpaOnnxBinary() string {
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

// DetectSenseVoiceModel searches for SenseVoice model in config and known locations.
func DetectSenseVoiceModel() string {
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

// EnsureSherpaOnnx ensures sherpa-onnx-offline binary is available.
func EnsureSherpaOnnx() (string, error) {
	sherpaPath := DetectSherpaOnnxBinary()
	if sherpaPath != "" {
		return sherpaPath, nil
	}
	configDir := config.GetConfigDir()
	sherpaOnnxVersion := "1.12.28"
	sherpaURL := fmt.Sprintf("https://github.com/k2-fsa/sherpa-onnx/releases/download/v%s/sherpa-onnx-v%s-linux-x64-gpu.tar.bz2", sherpaOnnxVersion, sherpaOnnxVersion)
	destDir := filepath.Join(configDir, "sherpa-onnx")
	topDir, err := DownloadAndExtractTarBz2(sherpaURL, destDir)
	if err != nil {
		return "", fmt.Errorf("failed to download sherpa-onnx: %w", err)
	}
	sherpaPath = filepath.Join(topDir, "bin", "sherpa-onnx-offline")
	if _, err := os.Stat(sherpaPath); os.IsNotExist(err) {
		return "", fmt.Errorf("sherpa-onnx-offline binary not found in extracted archive at %s", sherpaPath)
	}
	return sherpaPath, nil
}

// EnsureSenseVoiceModel ensures SenseVoice model is available.
func EnsureSenseVoiceModel() (string, error) {
	modelPath := DetectSenseVoiceModel()
	if modelPath != "" {
		return modelPath, nil
	}
	configDir := config.GetConfigDir()
	modelsDir := filepath.Join(configDir, "models", "sensevoice")
	modelURL := "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17.tar.bz2"
	topDir, err := DownloadAndExtractTarBz2(modelURL, modelsDir)
	if err != nil {
		return "", fmt.Errorf("failed to download SenseVoice model: %w", err)
	}
	modelPath = filepath.Join(topDir, "model.int8.onnx")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return "", fmt.Errorf("model file not found in extracted archive at %s", modelPath)
	}
	return modelPath, nil
}

// EnsureWhisperModel ensures a whisper model is available and returns its path.
func EnsureWhisperModel() (string, error) {
	cfg, _ := config.LoadAppConfig()
	if cfg.ModelPath != "" {
		if _, err := os.Stat(cfg.ModelPath); err == nil {
			return cfg.ModelPath, nil
		}
	}
	modelsDir := filepath.Join(config.GetConfigDir(), "models")
	home, _ := os.UserHomeDir()
	systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
	for _, m := range WhisperModels {
		if p := filepath.Join(modelsDir, m.Filename); FileExists(p) {
			return p, nil
		}
		if p := filepath.Join(systemModelsDir, m.Filename); FileExists(p) {
			return p, nil
		}
	}
	selected := WhisperModels[1]
	modelPath := filepath.Join(systemModelsDir, selected.Filename)
	modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.Filename)
	if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create models directory: %w", err)
	}
	if err := DownloadFile(modelPath, modelURL); err != nil {
		return "", fmt.Errorf("failed to download whisper model: %w", err)
	}
	return modelPath, nil
}

// DownloadFile downloads a URL to a local file path.
func DownloadFile(destPath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(destPath)
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

// DownloadAndExtractTarBz2 downloads a tar.bz2 file and extracts it.
// Returns the path of the top-level directory inside the archive.
func DownloadAndExtractTarBz2(url, destDir string) (string, error) {
	tmpFile := filepath.Join(destDir, "download.tar.bz2")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", err
	}
	fmt.Printf("Downloading from %s...\n", url)
	if err := DownloadFile(tmpFile, url); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tmpFile)
	f, err := os.Open(tmpFile)
	if err != nil {
		return "", err
	}
	defer f.Close()
	bz2Reader := bzip2.NewReader(f)
	tarReader := tar.NewReader(bz2Reader)
	var topDir string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar error: %w", err)
		}
		// Skip unsafe paths
		if strings.Contains(header.Name, "..") {
			continue
		}
		target := filepath.Join(destDir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return "", err
			}
			if topDir == "" {
				parts := strings.SplitN(header.Name, "/", 2)
				topDir = filepath.Join(destDir, parts[0])
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return "", err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return "", err
			}
			outFile.Close()
		}
	}
	if topDir == "" {
		return destDir, nil
	}
	return topDir, nil
}
