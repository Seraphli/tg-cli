package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Credentials struct {
	BotToken     string       `json:"botToken"`
	PairingAllow PairingAllow `json:"pairingAllow"`
	Port         int          `json:"port"`
}

type PairingAllow struct {
	IDs           []string `json:"ids"`
	DefaultChatID string   `json:"defaultChatId"`
}

var ConfigDir string // Set by root command PersistentPreRun

func GetConfigDir() string {
	if ConfigDir != "" {
		return ConfigDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tg-cli")
}

func GetCredentialsPath() string {
	return filepath.Join(GetConfigDir(), "credentials.json")
}

func ensureConfigDir() error {
	dir := GetConfigDir()
	return os.MkdirAll(dir, 0755)
}

func LoadCredentials() (Credentials, error) {
	if err := ensureConfigDir(); err != nil {
		return Credentials{}, err
	}
	path := GetCredentialsPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return Credentials{
			BotToken: "",
			PairingAllow: PairingAllow{
				IDs:           []string{},
				DefaultChatID: "",
			},
			Port: 12500,
		}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, err
	}
	if creds.PairingAllow.IDs == nil {
		creds.PairingAllow.IDs = []string{}
	}
	if creds.Port == 0 {
		creds.Port = 12500
	}
	return creds, nil
}

func SaveCredentials(creds Credentials) error {
	if err := ensureConfigDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	path := GetCredentialsPath()
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return nil
}

type AppConfig struct {
	WhisperPath string `json:"whisperPath"`
	ModelPath   string `json:"modelPath"`
	Language    string `json:"language"`
	FFmpegPath  string `json:"ffmpegPath"`
}

func GetConfigPath() string {
	return filepath.Join(GetConfigDir(), "config.json")
}

func LoadAppConfig() (AppConfig, error) {
	if err := ensureConfigDir(); err != nil {
		return AppConfig{}, err
	}
	path := GetConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return AppConfig{FFmpegPath: "ffmpeg"}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AppConfig{}, err
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	return cfg, nil
}

func SaveAppConfig(cfg AppConfig) error {
	if err := ensureConfigDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(GetConfigPath(), data, 0644)
}
