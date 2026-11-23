package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// InboundMessage is a Discord message adapted for the durable channel event.
type InboundMessage struct {
	ID         string
	Content    string
	Sender     string
	AuthorKind string
}

// InboundEvent identifies the provider room that produced a message.
type InboundEvent struct {
	ChannelName string
	RoomID      string
	Message     InboundMessage
}

type discordInboundState struct {
	mu    sync.Mutex
	rooms map[string]*string
	botID string
}

const discordUnknownRoomNotice = "This is not a project channel — I don't forward mail here."

// RegisterRoom adds a room once. Its initial cursor is retained for gateway
// Ready backfill; later ensure_room calls with a nil cursor do not erase it.
func (d *Discord) RegisterRoom(roomID string, afterID *string) error {
	id, err := parseSnowflake(roomID)
	if err != nil {
		return fmt.Errorf("invalid Discord channel ID: %w", err)
	}
	roomID = strconv.FormatUint(id, 10)
	var cursor *string
	if afterID != nil {
		value := *afterID
		cursor = &value
	}
	d.inbound.mu.Lock()
	defer d.inbound.mu.Unlock()
	if d.inbound.rooms == nil {
		d.inbound.rooms = make(map[string]*string)
	}
	if _, exists := d.inbound.rooms[roomID]; !exists {
		d.inbound.rooms[roomID] = cursor
	}
	return nil
}

// setDiscordBotID records the gateway's own user ID once, matching the source
// OnceLock behavior. Invalid IDs are ignored because gateway IDs are typed.
func (d *Discord) setDiscordBotID(botID string) {
	id, err := parseSnowflake(botID)
	if err != nil {
		return
	}
	d.inbound.mu.Lock()
	defer d.inbound.mu.Unlock()
	if d.inbound.botID == "" {
		d.inbound.botID = strconv.FormatUint(id, 10)
	}
}

// FetchSince returns at most the provider's next 100 messages after afterID.
// API order is preserved; cursor progression belongs to inbound storage.
func (d *Discord) FetchSince(ctx context.Context, roomID string, afterID *string) ([]InboundMessage, error) {
	room, err := parseSnowflake(roomID)
	if err != nil {
		return nil, fmt.Errorf("parse Discord channel ID: %w", err)
	}
	roomID = strconv.FormatUint(room, 10)
	query := url.Values{}
	query.Set("limit", "100")
	if afterID != nil {
		after, err := parseSnowflake(*afterID)
		if err != nil {
			return nil, fmt.Errorf("parse Discord after ID: %w", err)
		}
		query.Set("after", strconv.FormatUint(after, 10))
	}
	var messages []discordInboundAPIMessage
	if err := d.request(ctx, "GET", "/channels/"+roomID+"/messages?"+query.Encode(), nil, &messages); err != nil {
		return nil, fmt.Errorf("fetch Discord messages: %w", err)
	}
	if messages == nil {
		return nil, errors.New("Discord returned an invalid message list")
	}
	botID := d.discordBotID()
	result := make([]InboundMessage, 0, len(messages))
	for _, message := range messages {
		adapted, ownMessage, err := adaptDiscordInboundMessage(message, botID)
		if err != nil {
			return nil, err
		}
		if !ownMessage {
			result = append(result, adapted)
		}
	}
	return result, nil
}

// backfillDiscordRooms sends startup/reconnect backfill events for rooms with
// a valid stored cursor. Each room is isolated: a provider failure is logged
// without secrets and does not prevent backfill of later rooms.
func (d *Discord) backfillDiscordRooms(ctx context.Context, sink chan<- InboundEvent) error {
	type registeredRoom struct {
		id     string
		cursor string
	}
	d.inbound.mu.Lock()
	rooms := make([]registeredRoom, 0, len(d.inbound.rooms))
	for roomID, cursor := range d.inbound.rooms {
		if cursor == nil {
			continue
		}
		if _, err := parseSnowflake(*cursor); err != nil {
			continue
		}
		rooms = append(rooms, registeredRoom{id: roomID, cursor: *cursor})
	}
	d.inbound.mu.Unlock()

	for _, room := range rooms {
		if err := ctx.Err(); err != nil {
			return err
		}
		messages, err := d.FetchSince(ctx, room.id, &room.cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.WarnContext(ctx, "Discord room backfill failed", "room_id", room.id)
			continue
		}
		for _, message := range messages {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case sink <- InboundEvent{ChannelName: "discord", RoomID: room.id, Message: message}:
			}
		}
	}
	return nil
}

// dispatchDiscordMessage adapts one live gateway MESSAGE_CREATE payload. Only
// messages from this bot are discarded; other bots and webhooks are retained.
func (d *Discord) dispatchDiscordMessage(ctx context.Context, sink chan<- InboundEvent, raw json.RawMessage) error {
	var message discordInboundAPIMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return errors.New("decode Discord gateway message")
	}
	channelID, err := parseSnowflake(message.ChannelID)
	if err != nil {
		return errors.New("Discord gateway message has invalid channel ID")
	}
	roomID := strconv.FormatUint(channelID, 10)
	d.inbound.mu.Lock()
	_, known := d.inbound.rooms[roomID]
	botID := d.inbound.botID
	d.inbound.mu.Unlock()

	adapted, ownMessage, err := adaptDiscordInboundMessage(message, botID)
	if err != nil {
		return err
	}
	if ownMessage {
		return nil
	}
	if !known {
		if discordMessageMentions(message, botID) {
			if _, err := d.SendText(ctx, roomID, nil, discordUnknownRoomNotice); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.WarnContext(ctx, "Discord non-project mention reply failed", "room_id", roomID)
			}
		}
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sink <- InboundEvent{ChannelName: "discord", RoomID: roomID, Message: adapted}:
		return nil
	}
}

func (d *Discord) discordBotID() string {
	d.inbound.mu.Lock()
	defer d.inbound.mu.Unlock()
	return d.inbound.botID
}

type discordInboundAPIMessage struct {
	ID        string                 `json:"id"`
	ChannelID string                 `json:"channel_id"`
	Content   string                 `json:"content"`
	Author    discordInboundAuthor   `json:"author"`
	WebhookID *string                `json:"webhook_id"`
	Mentions  []discordInboundAuthor `json:"mentions"`
	Embeds    []discordInboundEmbed  `json:"embeds"`
}

type discordInboundAuthor struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type discordInboundEmbed struct {
	Author *struct {
		Name string `json:"name"`
	} `json:"author"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Fields      []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"fields"`
	Footer *struct {
		Text string `json:"text"`
	} `json:"footer"`
}

func adaptDiscordInboundMessage(message discordInboundAPIMessage, botID string) (InboundMessage, bool, error) {
	id, err := parseSnowflake(message.ID)
	if err != nil {
		return InboundMessage{}, false, errors.New("Discord message has invalid ID")
	}
	authorID, err := parseSnowflake(message.Author.ID)
	if err != nil {
		return InboundMessage{}, false, errors.New("Discord message has invalid author ID")
	}
	if botID != "" && strconv.FormatUint(authorID, 10) == botID {
		return InboundMessage{}, true, nil
	}
	authorKind := "human"
	if message.WebhookID != nil {
		authorKind = "webhook"
	} else if message.Author.Bot {
		authorKind = "bot"
	}
	return InboundMessage{
		ID: strconv.FormatUint(id, 10), Content: discordInboundContent(message.Content, message.Embeds),
		Sender: message.Author.Username, AuthorKind: authorKind,
	}, false, nil
}

func discordMessageMentions(message discordInboundAPIMessage, botID string) bool {
	if botID == "" {
		return false
	}
	for _, mention := range message.Mentions {
		id, err := parseSnowflake(mention.ID)
		if err == nil && strconv.FormatUint(id, 10) == botID {
			return true
		}
	}
	return false
}

func discordInboundContent(content string, embeds []discordInboundEmbed) string {
	parts := make([]string, 0, len(embeds)+1)
	appendTrimmedDiscordPart(&parts, content)
	for _, embed := range embeds {
		embedParts := make([]string, 0, 2+len(embed.Fields))
		if embed.Author != nil {
			appendTrimmedDiscordPart(&embedParts, embed.Author.Name)
		}
		appendTrimmedDiscordPart(&embedParts, embed.Title)
		appendTrimmedDiscordPart(&embedParts, embed.Description)
		for _, field := range embed.Fields {
			name, value := strings.TrimSpace(field.Name), strings.TrimSpace(field.Value)
			switch {
			case name == "" && value == "":
			case name == "":
				embedParts = append(embedParts, value)
			case value == "":
				embedParts = append(embedParts, name)
			default:
				embedParts = append(embedParts, name+": "+value)
			}
		}
		if embed.Footer != nil {
			appendTrimmedDiscordPart(&embedParts, embed.Footer.Text)
		}
		if len(embedParts) != 0 {
			parts = append(parts, strings.Join(embedParts, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}

func appendTrimmedDiscordPart(parts *[]string, value string) {
	if value = strings.TrimSpace(value); value != "" {
		*parts = append(*parts, value)
	}
}
