package types

import (
	"sync"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	tele "gopkg.in/telebot.v3"
)

// BotState holds all store instances and shared state for the bot.
type BotState struct {
	Bot   *tele.Bot
	Creds *config.Credentials

	Pages           *stores.PageCacheStore
	PendingPerms    *stores.PendingPermStore
	ToolNotifs      *stores.ToolNotifyStore
	PendingFiles    *stores.PendingFileStore
	SessionState    *stores.SessionStateStore
	SessionCounts   *stores.SessionCountStore
	MergeBuffers    *stores.MergeBufferStore
	InjectQueue     *stores.InjectQueueStore
	InjectConfirm   *stores.InjectConfirmStore
	CronJobs        *stores.CronJobStore
	ReactionTracker *stores.ReactionTrackerStore
	HookRunning     *stores.HookRunningStateStore
	StopCooldown    *stores.StopCooldownStore
	SessionWatch    *stores.SessionWatchStore
	ToolUseMsgs     *stores.ToolUseMsgStore
	CommandStats    *stores.CommandStatsStore
	SessionEvents   *stores.SessionEventStore
	AtChannels      *stores.AtChannelStore

	LaunchPending         sync.Map
	TmuxPaneCache         sync.Map
	PendingExitKill       sync.Map
	PendingUpgradeRestart sync.Map
	HookSessionLocks      sync.Map
	VersionNotified       sync.Map
	UnbindMenuItems       sync.Map
	BindMenuItems         sync.Map
	SettingsMenuMsgs      sync.Map
}
