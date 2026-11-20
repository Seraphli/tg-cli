package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// ChatInfo is the part of Telegram's getChat result the bind rules need.
type ChatInfo struct {
	Type    string `json:"type"`
	IsForum bool   `json:"is_forum"`
	Title   string `json:"title"`
}

// FetchChatInfo makes ONE raw getChat call. telebot v3.3.8 Chat has no is_forum field (chat.go:15 is in
// User), so the result is decoded here. A Telegram error is returned exactly as bot.Raw returned it.
func FetchChatInfo(b *tele.Bot, chatID int64) (ChatInfo, error) {
	data, err := b.Raw("getChat", map[string]string{"chat_id": strconv.FormatInt(chatID, 10)})
	if err != nil {
		return ChatInfo{}, err
	}
	var resp struct {
		Result ChatInfo `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ChatInfo{}, fmt.Errorf("getChat response decode: %w", err)
	}
	return resp.Result, nil
}

// BindForumTopic binds session `name` to its own topic in the forum supergroup chatID and returns the
// stored route. The topic recorded for (chatID, name) is reused; otherwise one is created, named after the
// session, and recorded. The record and the route change together in one config.UpdateRoutes, so a change
// made during the network call is kept.
func BindForumTopic(b *tele.Bot, name string, chatID int64, title string) (config.NameRoute, error) {
	routes, err := config.LoadRoutes()
	if err != nil {
		return config.NameRoute{}, err
	}
	key := config.ForumTopicKey(chatID, name)
	topicID := routes.ForumTopics[key]
	if topicID == 0 {
		topic, err := b.CreateTopic(&tele.Chat{ID: chatID}, &tele.Topic{Name: name})
		if err != nil {
			return config.NameRoute{}, err
		}
		if topic == nil || topic.ThreadID == 0 {
			return config.NameRoute{}, fmt.Errorf("createForumTopic returned no thread id")
		}
		topicID = topic.ThreadID
		logger.Info(fmt.Sprintf("Forum topic created: chat=%d name=%s thread=%d", chatID, name, topicID))
	}
	route := config.NameRoute{ChatID: chatID, TopicID: topicID, Title: title}
	if err := config.UpdateRoutes(func(r *config.Routes) bool {
		r.ForumTopics[key] = topicID
		r.NameRouteMap[name] = route
		return true
	}); err != nil {
		return config.NameRoute{}, err
	}
	return route, nil
}

// FillEmptyRouteTitles is the startup background fill: getChat every chat whose route title is empty
// (the first read only lists chat ids and is never written back), then config.FillEmptyTitles
// changes the current routes once and fills only titles that are still empty. A failed getChat leaves the
// title empty; the next startup retries. It ends with one summary line on every normal end, also with
// zero chats, so a run of the real bot proves that runBot started it.
func FillEmptyRouteTitles(ctx context.Context, b *tele.Bot) {
	routes, err := config.LoadRoutes()
	if err != nil {
		logger.Warn("route title fill: " + err.Error())
		return
	}
	seen := make(map[int64]bool)
	titles := make(map[int64]string)
	failed := 0
	for _, r := range routes.NameRouteMap {
		if r.Title != "" || seen[r.ChatID] {
			continue
		}
		seen[r.ChatID] = true
		if ctx.Err() != nil {
			return
		}
		info, err := FetchChatInfo(b, r.ChatID)
		if err != nil {
			failed++
			logger.Warn(fmt.Sprintf("route title fill: chat=%d err=%v", r.ChatID, err))
			continue
		}
		titles[r.ChatID] = info.Title
	}
	if len(titles) > 0 {
		if err := config.FillEmptyTitles(titles); err != nil {
			logger.Warn("route title fill: " + err.Error())
			return
		}
	}
	logger.Info(fmt.Sprintf("Route title fill done: chats=%d fetched=%d failed=%d", len(seen), len(titles), failed))
}
