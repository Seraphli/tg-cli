package helpers

// exports for the external test package helpers_test, which imports cmd/handlers; a test in package helpers cannot, handlers imports helpers
var (
	LiveStart    = liveStart
	LiveTurn     = liveTurn
	LiveWait     = liveWait
	LiveFail     = liveFail
	HasReplyLine = hasReplyLine
	ViewportNow  = viewportNow
	CCBoxNow     = ccBoxNow
)
