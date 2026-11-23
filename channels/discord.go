package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	discordDefaultAPI     = "https://discord.com/api/v10"
	discordMaxResponse    = 1 << 20
	discordEmbedTitle     = 256
	discordEmbedBody      = 4000
	discordEmbedColor     = 0x5865F2
	discordRequestTimeout = 30 * time.Second
	discordUserAgent      = "DiscordBot (https://github.com/nitecon/amux, 0.0.0)"
)

// DiscordConfig configures the outbound Discord REST adapter.
type DiscordConfig struct {
	Token      string
	GuildID    string
	CategoryID string
	APIBaseURL string
	GatewayURL string
	HTTPClient *http.Client
}

// Discord sends messages and provisions channels through Discord's REST API.
type Discord struct {
	token      string
	guildID    string
	categoryID string
	apiBaseURL string
	gatewayURL string
	client     *http.Client
	inbound    discordInboundState
	runMu      sync.Mutex
}

// NewDiscord validates configuration without making a provider request.
func NewDiscord(config DiscordConfig) (*Discord, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("Discord bot token is required")
	}
	guildID, err := parseSnowflake(config.GuildID)
	if err != nil {
		return nil, fmt.Errorf("invalid Discord guild ID: %w", err)
	}
	config.GuildID = strconv.FormatUint(guildID, 10)
	if config.CategoryID != "" {
		categoryID, err := parseSnowflake(config.CategoryID)
		if err != nil {
			return nil, fmt.Errorf("invalid Discord category ID: %w", err)
		}
		config.CategoryID = strconv.FormatUint(categoryID, 10)
	}
	baseURL := config.APIBaseURL
	if baseURL == "" {
		baseURL = discordDefaultAPI
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("invalid Discord API base URL")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("Discord API base URL must use HTTPS")
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, errors.New("invalid Discord API base URL")
	}
	gatewayURL := ""
	if config.GatewayURL != "" {
		gatewayURL, err = normalizeGatewayURL(config.GatewayURL)
		if err != nil {
			return nil, fmt.Errorf("invalid Discord Gateway URL: %w", err)
		}
	}
	clientCopy := *http.DefaultClient
	if config.HTTPClient != nil {
		clientCopy = *config.HTTPClient
	}
	if clientCopy.Timeout <= 0 {
		clientCopy.Timeout = discordRequestTimeout
	}
	// Never forward the bot authorization header to a redirect target.
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Discord{
		token: config.Token, guildID: config.GuildID, categoryID: config.CategoryID,
		apiBaseURL: baseURL, gatewayURL: gatewayURL, client: &clientCopy,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseSnowflake(value string) (uint64, error) {
	parsed := strings.TrimPrefix(value, "+")
	id, err := strconv.ParseUint(parsed, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("must be a positive decimal snowflake")
	}
	return id, nil
}

func (d *Discord) EnsureRoom(ctx context.Context, project string) (string, error) {
	if project == "" {
		return "", errors.New("Discord room name is required")
	}
	path := "/guilds/" + d.guildID + "/channels"
	var channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := d.request(ctx, http.MethodGet, path, nil, &channels); err != nil {
		return "", fmt.Errorf("list Discord guild channels: %w", err)
	}
	if channels == nil {
		return "", errors.New("Discord returned an invalid channel list")
	}
	for _, channel := range channels {
		if channel.Name == project {
			id, err := parseSnowflake(channel.ID)
			if err != nil {
				return "", errors.New("Discord returned an invalid channel ID")
			}
			roomID := strconv.FormatUint(id, 10)
			if err := d.RegisterRoom(roomID, nil); err != nil {
				return "", err
			}
			return roomID, nil
		}
	}
	create := struct {
		Name     string  `json:"name"`
		Type     int     `json:"type"`
		ParentID *string `json:"parent_id,omitempty"`
	}{Name: project, Type: 0}
	if d.categoryID != "" {
		create.ParentID = &d.categoryID
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := d.request(ctx, http.MethodPost, path, create, &channel); err != nil {
		return "", fmt.Errorf("create Discord project channel: %w", err)
	}
	id, err := parseSnowflake(channel.ID)
	if err != nil {
		return "", errors.New("Discord returned an invalid channel ID")
	}
	roomID := strconv.FormatUint(id, 10)
	if err := d.RegisterRoom(roomID, nil); err != nil {
		return "", err
	}
	return roomID, nil
}

func (d *Discord) Send(ctx context.Context, roomID string, parentID *string, message OutboundMessage) (string, error) {
	room, reference, err := discordTarget(roomID, parentID)
	if err != nil {
		return "", err
	}
	return d.sendMessage(ctx, room, discordCreateMessage{Embeds: []discordEmbed{buildDiscordEmbed(message)}, MessageReference: reference})
}

func (d *Discord) SendText(ctx context.Context, roomID string, parentID *string, content string) (string, error) {
	room, reference, err := discordTarget(roomID, parentID)
	if err != nil {
		return "", err
	}
	return d.sendMessage(ctx, room, discordCreateMessage{Content: &content, MessageReference: reference})
}

func discordTarget(roomID string, parentID *string) (string, *discordMessageReference, error) {
	room, err := parseSnowflake(roomID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid Discord channel ID: %w", err)
	}
	canonicalRoom := strconv.FormatUint(room, 10)
	if parentID == nil {
		return canonicalRoom, nil, nil
	}
	parent, err := parseSnowflake(*parentID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid Discord parent message ID: %w", err)
	}
	return canonicalRoom, &discordMessageReference{
		MessageID: strconv.FormatUint(parent, 10),
		ChannelID: canonicalRoom,
	}, nil
}

func (d *Discord) sendMessage(ctx context.Context, roomID string, request discordCreateMessage) (string, error) {
	var response struct {
		ID string `json:"id"`
	}
	if err := d.request(ctx, http.MethodPost, "/channels/"+roomID+"/messages", request, &response); err != nil {
		return "", fmt.Errorf("send Discord message: %w", err)
	}
	id, err := parseSnowflake(response.ID)
	if err != nil {
		return "", errors.New("Discord returned an invalid message ID")
	}
	return strconv.FormatUint(id, 10), nil
}

type discordCreateMessage struct {
	Content          *string                  `json:"content,omitempty"`
	Embeds           []discordEmbed           `json:"embeds,omitempty"`
	MessageReference *discordMessageReference `json:"message_reference,omitempty"`
}

type discordMessageReference struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

type discordEmbed struct {
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Color       int                `json:"color"`
	Author      discordEmbedAuthor `json:"author"`
	Timestamp   *string            `json:"timestamp,omitempty"`
}

type discordEmbedAuthor struct {
	Name string `json:"name"`
}

func buildDiscordEmbed(message OutboundMessage) discordEmbed {
	embed := discordEmbed{
		Title:       truncateRunes(message.Subject, discordEmbedTitle),
		Description: truncateRunes(message.Body, discordEmbedBody),
		Color:       discordEmbedColor,
		Author:      discordEmbedAuthor{Name: message.AgentID + " · " + message.Hostname},
	}
	seconds := message.EventAt / 1000 // Go integer division matches Rust truncation toward zero.
	timestamp := time.Unix(seconds, 0).UTC()
	if timestamp.Year() >= 0 && timestamp.Year() <= 9999 {
		value := timestamp.Format(time.RFC3339)
		embed.Timestamp = &value
	}
	return embed
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}

func (d *Discord) request(ctx context.Context, method, path string, body, result any) error {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New("encode Discord request")
		}
		requestBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.apiBaseURL+path, requestBody)
	if err != nil {
		return errors.New("build Discord request")
	}
	req.Header.Set("Authorization", "Bot "+d.token)
	req.Header.Set("User-Agent", discordUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	response, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("Discord request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, discordMaxResponse+1))
	if err != nil {
		return fmt.Errorf("read Discord response: %w", err)
	}
	if len(data) > discordMaxResponse {
		return errors.New("Discord response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Discord returned HTTP %d", response.StatusCode)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("decode Discord response")
	}
	return nil
}
