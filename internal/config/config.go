package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

type NameRoute struct {
	ChatID  int64 `json:"chatId"`
	TopicID int   `json:"topicId,omitempty"`
}

type Credentials struct {
	BotToken     string               `json:"botToken"`
	PairingAllow PairingAllow         `json:"pairingAllow"`
	Port         int                  `json:"port"`
	NameRouteMap map[string]NameRoute `json:"nameRouteMap,omitempty"`
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
	if creds.NameRouteMap == nil {
		creds.NameRouteMap = make(map[string]NameRoute)
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
	WhisperPath        string   `json:"whisperPath"`
	ModelPath          string   `json:"modelPath"`
	Language           string   `json:"language"`
	FFmpegPath         string   `json:"ffmpegPath"`
	WhisperPrompt      string   `json:"whisperPrompt"`
	VoicePrefix        string   `json:"voicePrefix"`
	ToolNotifyList     []string `json:"toolNotifyList,omitempty"`
	ToolNotifyEnabled  *bool    `json:"toolNotifyEnabled,omitempty"`
	ClaudeCommand      string   `json:"claudeCommand"`
	DefaultSessionName string   `json:"defaultSessionName"`
	DefaultWorkDir     string   `json:"defaultWorkDir"`
	VoiceEngine        string   `json:"voiceEngine"`
	SherpaOnnxPath     string   `json:"sherpaOnnxPath"`
	SenseVoiceModelPath string `json:"senseVoiceModelPath"`
	VoiceRetainCount    int    `json:"voiceRetainCount,omitempty"`
	CWDSource           string `json:"cwdSource,omitempty"`
}

// appConfigCache stores the last loaded config for dynamic reload support.
var appConfigCache atomic.Pointer[AppConfig]
var lastLoadMtime atomic.Value // stores time.Time

func GetConfigPath() string {
	return filepath.Join(GetConfigDir(), "config.json")
}

func loadAppConfigFromDisk() (AppConfig, error) {
	if err := ensureConfigDir(); err != nil {
		return AppConfig{}, err
	}
	path := GetConfigPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return AppConfig{FFmpegPath: "ffmpeg", VoicePrefix: "🗣️", ClaudeCommand: "claude", DefaultSessionName: "tg-cli", DefaultWorkDir: filepath.Join(GetConfigDir(), "workspace"), VoiceEngine: "whisper"}, nil
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
	if cfg.VoicePrefix == "" {
		cfg.VoicePrefix = "🗣️"
	}
	if cfg.ClaudeCommand == "" {
		cfg.ClaudeCommand = "claude"
	}
	if cfg.DefaultSessionName == "" {
		cfg.DefaultSessionName = "tg-cli"
	}
	if cfg.DefaultWorkDir == "" {
		cfg.DefaultWorkDir = filepath.Join(GetConfigDir(), "workspace")
	}
	if cfg.VoiceEngine == "" {
		cfg.VoiceEngine = "whisper"
	}
	if cfg.VoiceRetainCount == 0 {
		cfg.VoiceRetainCount = 5
	}
	if cfg.CWDSource == "" {
		cfg.CWDSource = "tmux"
	}
	return cfg, nil
}

// LoadAppConfig returns config from cache if available and file unchanged, otherwise loads from disk.
func LoadAppConfig() (AppConfig, error) {
	if cached := appConfigCache.Load(); cached != nil {
		if info, err := os.Stat(GetConfigPath()); err == nil {
			if stored, ok := lastLoadMtime.Load().(time.Time); ok && !info.ModTime().After(stored) {
				return *cached, nil
			}
		} else {
			return *cached, nil
		}
		appConfigCache.Store(nil)
	}
	cfg, err := loadAppConfigFromDisk()
	if err != nil {
		return cfg, err
	}
	if info, err := os.Stat(GetConfigPath()); err == nil {
		lastLoadMtime.Store(info.ModTime())
	}
	appConfigCache.Store(&cfg)
	return cfg, nil
}

// ReloadAppConfig clears the cache and reloads config from disk.
func ReloadAppConfig() (AppConfig, error) {
	appConfigCache.Store(nil)
	return LoadAppConfig()
}

// UpdateAppConfigCache updates the in-memory cache with the given config.
func UpdateAppConfigCache(cfg AppConfig) {
	appConfigCache.Store(&cfg)
}

// MigrateChat updates all credential references from oldID to newID.
func MigrateChat(oldID, newID int64) error {
	creds, err := LoadCredentials()
	if err != nil {
		return err
	}
	migrated := false
	for k, v := range creds.NameRouteMap {
		if v.ChatID == oldID {
			v.ChatID = newID
			creds.NameRouteMap[k] = v
			migrated = true
		}
	}
	if creds.PairingAllow.DefaultChatID == strconv.FormatInt(oldID, 10) {
		creds.PairingAllow.DefaultChatID = strconv.FormatInt(newID, 10)
		migrated = true
	}
	if !migrated {
		return nil
	}
	return SaveCredentials(creds)
}

func SaveAppConfig(cfg AppConfig) error {
	if err := ensureConfigDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(GetConfigPath(), data, 0644); err != nil {
		return err
	}
	UpdateAppConfigCache(cfg)
	if info, err := os.Stat(GetConfigPath()); err == nil {
		lastLoadMtime.Store(info.ModTime())
	}
	return nil
}
