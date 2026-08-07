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

	Port      int    // resolved bot HTTP port; used to render @ channel reply/end CLI instructions
	ConfigDir string // resolved config dir; used to render @ channel reply/end CLI instructions

	Pages           *stores.PageCacheStore
	SessionState    *stores.SessionStateStore
	SessionCounts   *stores.SessionCountStore
	MergeBuffers    *stores.MergeBufferStore
	InjectQueue     *stores.InjectQueueStore
	InjectConfirm   *stores.InjectConfirmStore
	InjectRoute     *stores.InjectRouteStore // event-driven inject-queue routing (MD-final trigger, R2)
	CronJobs        *stores.CronJobStore
	ReactionTracker *stores.ReactionTrackerStore
	HookRunning     *stores.HookRunningStateStore
	StopCooldown    *stores.StopCooldownStore
	SessionWatch    *stores.SessionWatchStore
	ToolUseMsgs     *stores.ToolUseMsgStore
	CommandStats    *stores.CommandStatsStore
	SessionEvents   *stores.SessionEventStore // Hook FIFO (per-session serialized; uses drain/wait/await)
	MessageQueue    *stores.SessionEventStore // Message FIFO (per-session serialized; owns TG send/edit I/O + MsgIDMap mutations)
	MsgIDMap        *stores.MsgIDMap          // internal-id -> (TG-msg-id, sessionID); written/read/deleted ONLY on the Message FIFO
	AtChannels      *stores.AtChannelStore
	CompactTools    *stores.CompactToolStore
	Streams         *stores.StreamStore
	PendingWait     *stores.PendingWaitStore
	PendingMsgStore *stores.PendingMsgStore
	BusyStatus      *stores.BusyStatusStore

	// f29 D: per-target previous running state for busy→idle 401 detection. Owned by the SERIAL
	// runBusyTick loop (ticker-serialized, no lock needed); doubles as the once-per-stall dedup (rearm on
	// false→true). Lazily initialized in runBusyTick.
	BusyPrevRunning map[string]bool

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
