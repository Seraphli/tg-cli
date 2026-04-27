package helpers

import (
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/notify"
)

func QueuedInject(
	events *stores.SessionEventStore,
	sessionState *stores.SessionStateStore,
	target injector.TmuxTarget,
	text string,
	submit ...bool,
) error {
	sid, _ := sessionState.FindByTarget(notify.FormatPaneID(injector.FormatTarget(target)))
	return events.Dispatch(sid, "inject:raw", func() error {
		return injector.InjectText(target, text, submit...)
	})
}
