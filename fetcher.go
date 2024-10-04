package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const (
	masterURL         = "http://master.powerline.io"
	opcodeHelloV4     = 0xBF
	opcodeCsPing      = 0x00
	opcodeScPong      = 0x01
	opcodeConfig      = 0xA0
	opcodeConfig2     = 0xB0
	opcodeLeaderboard = 0xA5
	gameScale         = 10.0
	websocketTimeout  = 36 * time.Second
	handshakeTimeout  = 24 * time.Second
)

var (
	regions         = []string{"eu", "us", "as"}
	countryToRegion = map[string]string{
		"DE": "eu", "JP": "as", "US": "us",
	}
	regionNames = map[string]string{
		"eu": "🌍 Europe & Africa",
		"us": "🌎 America",
		"as": "🌏 Asia & Oceania",
	}
)

type (
	WebhookConfig struct {
		URL       string
		MessageID string
	}

	RoomInfo struct {
		Region     string
		Server     string
		RoomCode   string
		RoomNumber string
		Latency    time.Duration
		IsOnline   bool
		ArenaSize  int
		Players    []Player
		TopPlayer  string
		TopScore   uint32
		RawData    string
	}

	Player struct {
		Name  string
		Score uint32
	}

	DiscordEmbed struct {
		Title       string         `json:"title"`
		Description string         `json:"description"`
		Color       int            `json:"color"`
		Fields      []DiscordField `json:"fields"`
		Footer      DiscordFooter  `json:"footer"`
		Timestamp   string         `json:"timestamp"`
	}

	DiscordField struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline"`
	}

	DiscordFooter struct {
		Text string `json:"text"`
	}

	DiscordWebhookPayload struct {
		Content     interface{}    `json:"content"`
		Embeds      []DiscordEmbed `json:"embeds"`
		Attachments []interface{}  `json:"attachments"`
	}
)

func fetchRoomCodes(region string, wg *sync.WaitGroup, results chan<- []RoomInfo) {
	defer wg.Done()

	client := &http.Client{Timeout: 10 * time.Second}

	var roomInfos []RoomInfo
	for country, regionCode := range countryToRegion {
		if regionCode == region {
			req, err := http.NewRequest("PUT", masterURL, strings.NewReader(country))
			if err != nil {
				log.Printf("Error creating request for %s (%s): %v", region, country, err)
				continue
			}

			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("User-Agent", "RoomCodeFetcher/1.0")

			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Error fetching room codes for %s (%s): %v", region, country, err)
				continue
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Error reading response for %s (%s): %v", region, country, err)
				continue
			}

			roomInfos = append(roomInfos, parseRoomInfo(region, string(body))...)
		}
	}
	results <- roomInfos
}

func parseRoomInfo(region, response string) []RoomInfo {
	rooms := strings.Split(response, ",")
	var roomInfos []RoomInfo

	for _, room := range rooms {
		parts := strings.Split(room, "!")
		if len(parts) != 2 {
			log.Printf("Unexpected response format for %s: %s", region, room)
			continue
		}

		serverInfo := strings.Split(parts[0], "/")
		server := serverInfo[0]
		roomNumber := "0"
		if len(serverInfo) > 1 {
			roomNumber = serverInfo[1]
		}
		roomCode := parts[1]

		serverAddress := strings.Split(server, ":")[0]

		gameInfo, err := fetchGameInfo(serverAddress)
		if err != nil {
			log.Printf("Error fetching game info for %s: %v", serverAddress, err)
			roomInfo := RoomInfo{
				Region: region, Server: server, RoomCode: roomCode,
				RoomNumber: roomNumber, IsOnline: false, RawData: room,
			}
			roomInfos = append(roomInfos, roomInfo)
			continue
		}

		roomInfo := RoomInfo{
			Region: region, Server: server, RoomCode: roomCode,
			RoomNumber: roomNumber, Latency: gameInfo.Latency,
			IsOnline: true, ArenaSize: gameInfo.ArenaSize,
			TopPlayer: gameInfo.TopPlayer, TopScore: gameInfo.TopScore,
			Players: gameInfo.Players, RawData: room,
		}

		log.Printf("Room %s in region %s has %d players", roomInfo.RoomCode, region, len(roomInfo.Players))
		roomInfos = append(roomInfos, roomInfo)
	}

	return roomInfos
}

func fetchGameInfo(serverAddress string) (*RoomInfo, error) {
	ports := []int{9181, 9182, 9183, 9184, 9185, 9186, 9187, 9188, 9189, 9190, 9191, 9081, 9082, 9083, 9084, 9085, 9086, 9087, 9088, 9089, 9090, 9091, 9092, 9093, 9094, 9095, 9096}
	var lastErr error

	for _, port := range ports {
		wsURL := fmt.Sprintf("wss://%s.powerline.io:%d", strings.Replace(serverAddress, ".", "-", -1), port)
		gameInfo, err := attemptConnection(wsURL)
		if err == nil {
			return gameInfo, nil
		}
		lastErr = err
		log.Printf("Failed to connect to %s: %v. \nTrying next port.", wsURL, err)
	}

	return nil, fmt.Errorf("failed to connect to server %s on all ports: %v", serverAddress, lastErr)
}

func attemptConnection(wsURL string) (*RoomInfo, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %v", err)
	}

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: handshakeTimeout,
	}

	headers := http.Header{
		"User-Agent": {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/93.0.4577.63 Safari/537.36"},
		"Origin":     {"https://powerline.io"},
	}

	c, _, err := dialer.Dial(u.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("websocket dial error: %v", err)
	}
	defer c.Close()

	gameInfo := &RoomInfo{}

	if err := sendHello(c); err != nil {
		return nil, fmt.Errorf("error sending HELLO: %v", err)
	}

	pingStart := time.Now()
	if err := sendPing(c); err != nil {
		return nil, fmt.Errorf("error sending PING: %v", err)
	}

	if err := c.SetReadDeadline(time.Now().Add(websocketTimeout)); err != nil {
		return nil, fmt.Errorf("error setting read deadline: %v", err)
	}

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				return nil, fmt.Errorf("websocket closed unexpectedly: %v", err)
			}
			return nil, fmt.Errorf("error reading message: %v", err)
		}

		if len(message) == 0 {
			continue
		}

		switch message[0] {
		case opcodeConfig, opcodeConfig2:
			processConfig(message, gameInfo)
		case opcodeLeaderboard:
			processLeaderboard(message, gameInfo)
		case opcodeScPong:
			gameInfo.Latency = time.Since(pingStart)
		}

		if gameInfo.ArenaSize != 5000 && gameInfo.Latency != 0 && gameInfo.Players != nil {
			gameInfo.IsOnline = true
			return gameInfo, nil
		}
	}
}

func sendHello(c *websocket.Conn) error {
	buf := make([]byte, 5)
	buf[0] = opcodeHelloV4
	binary.LittleEndian.PutUint16(buf[1:], uint16(math.Round(398/gameScale)))
	binary.LittleEndian.PutUint16(buf[3:], uint16(math.Round(842/gameScale)))
	return c.WriteMessage(websocket.BinaryMessage, buf)
}

func sendPing(c *websocket.Conn) error {
	return c.WriteMessage(websocket.BinaryMessage, []byte{opcodeCsPing})
}

func processConfig(message []byte, gameInfo *RoomInfo) {
	if len(message) < 5 {
		return
	}
	arenaSide := math.Float32frombits(binary.LittleEndian.Uint32(message[1:5]))
	gameInfo.ArenaSize = int(math.Round(float64(arenaSide)))
}

func processLeaderboard(message []byte, gameInfo *RoomInfo) {
	offset := 1
	gameInfo.Players = []Player{}
	for offset+6 <= len(message) {
		id := binary.LittleEndian.Uint16(message[offset:])
		offset += 2
		if id == 0 {
			break
		}
		score := binary.LittleEndian.Uint32(message[offset:])
		offset += 4
		nick, newOffset := getString(message, offset)
		offset = newOffset

		player := Player{Name: nick, Score: score}
		gameInfo.Players = append(gameInfo.Players, player)

		if gameInfo.TopScore < score {
			gameInfo.TopScore = score
			gameInfo.TopPlayer = nick
		}
	}
	sort.Slice(gameInfo.Players, func(i, j int) bool {
		return gameInfo.Players[i].Score > gameInfo.Players[j].Score
	})

	log.Printf("Processed leaderboard for room %s. Player count: %d", gameInfo.RoomCode, len(gameInfo.Players))
}

func getString(data []byte, offset int) (string, int) {
	var utf16Chars []uint16
	for offset+2 <= len(data) {
		v := binary.LittleEndian.Uint16(data[offset:])
		offset += 2
		if v == 0 {
			break
		}
		utf16Chars = append(utf16Chars, v)
	}
	return string(utf16.Decode(utf16Chars)), offset
}

func createLeaderboardEmbed(allRooms map[string][]RoomInfo) DiscordEmbed {
	embed := DiscordEmbed{
		Title:       "Powerline.io Leaderboard 🌟",
		Description: "🥇 **Top 10 players** for each region. Who will dominate the arena? 🏆",
		Color:       16766720,
		Footer: DiscordFooter{
			Text: "Leaderboard updates every few minutes.",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	for _, region := range regions {
		rooms, ok := allRooms[region]
		if !ok || len(rooms) == 0 {
			embed.Fields = append(embed.Fields, DiscordField{
				Name:   regionNames[region],
				Value:  "No players",
				Inline: true,
			})
			continue
		}

		var allPlayers []Player
		totalScore := uint32(0)
		for _, room := range rooms {
			for _, player := range room.Players {
				if player.Name == "" {
					player.Name = "<Unnamed>"
				}
				allPlayers = append(allPlayers, player)
				totalScore += player.Score
			}
		}

		if len(allPlayers) == 0 {
			embed.Fields = append(embed.Fields, DiscordField{
				Name:   regionNames[region],
				Value:  "🕸️ **Wrong Port/Empty Room** 🚧",
				Inline: true,
			})
			continue
		}

		sort.Slice(allPlayers, func(i, j int) bool {
			return allPlayers[i].Score > allPlayers[j].Score
		})

		var leaderboard strings.Builder
		leaderboard.WriteString(fmt.Sprintf("💯 **Total Score:** %d\n\n", totalScore))
		for i := 0; i < 10 && i < len(allPlayers); i++ {
			player := allPlayers[i]
			scoreStr := fmt.Sprintf("- %d", player.Score)
			if player.Score == 0 {
				scoreStr = ""
			}
			leaderboard.WriteString(getLeaderboardEntry(i, player.Name, scoreStr))
		}

		embed.Fields = append(embed.Fields, DiscordField{
			Name:   regionNames[region],
			Value:  leaderboard.String(),
			Inline: true,
		})
	}

	return embed
}

func getLeaderboardEntry(position int, name, score string) string {
	switch position {
	case 0:
		return fmt.Sprintf("🥇 **%s** %s\n", name, score)
	case 1:
		return fmt.Sprintf("🥈 **%s** %s\n", name, score)
	case 2:
		return fmt.Sprintf("🥉 **%s** %s\n", name, score)
	case 9:
		return fmt.Sprintf("💀 **%s** %s\n", name, score)
	default:
		return fmt.Sprintf("\u202F\u202F%s **%s** %s\n", string('➍'+rune(position-3)), name, score)
	}
}

func createDiscordPayload(allRooms map[string][]RoomInfo) DiscordWebhookPayload {
	totalRooms := 0
	downRooms := 0
	serverStatusEmbed := DiscordEmbed{
		Title:       "Powerline.io Server Status 📈",
		Description: "Real-time data for all regions.",
		Footer: DiscordFooter{
			Text: "Data updates every few minutes.",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	for _, region := range regions {
		rooms, ok := allRooms[region]
		if !ok || len(rooms) == 0 {
			serverStatusEmbed.Fields = append(serverStatusEmbed.Fields, DiscordField{
				Name:   regionNames[region],
				Value:  "❌ No rooms available",
				Inline: true,
			})
			continue
		}

		for i, room := range rooms {
			totalRooms++
			status := "❌ Down"
			latency := "N/A"
			roomCodeLink := fmt.Sprintf("[%s](https://powerline.io/#%s)", room.RoomCode, room.RoomCode)
			arenaSize := "Unknown"

			if room.IsOnline {
				status = "✅ Online"
				latency = fmt.Sprintf("%.2fms", float64(room.Latency.Microseconds())/1000)
				arenaSize = fmt.Sprintf("%dx%d", room.ArenaSize, room.ArenaSize)
			} else {
				downRooms++
			}

			roomName := regionNames[region]
			if i > 0 {
				roomName += fmt.Sprintf(" (Room %d)", i+1)
			}

			serverStatusEmbed.Fields = append(serverStatusEmbed.Fields, DiscordField{
				Name: roomName,
				Value: fmt.Sprintf("🏟️ **Arena size:** %s\n👥 **Players:** %d 🚧\n\n"+
					"🔗 **Room Code:** %s\n🏓 **Ping:** %s\n🚦 **Status:** %s",
					arenaSize, len(room.Players), roomCodeLink, latency, status),
				Inline: true,
			})
		}
	}

	serverStatusEmbed.Color = getStatusColor(downRooms, totalRooms)

	leaderboardEmbed := createLeaderboardEmbed(allRooms)

	return DiscordWebhookPayload{
		Embeds: []DiscordEmbed{serverStatusEmbed, leaderboardEmbed},
	}
}

func getStatusColor(downRooms, totalRooms int) int {
	if totalRooms == 0 {
		return 0xFF0000
	}

	downRatio := float64(downRooms) / float64(totalRooms)
	switch {
	case downRatio == 0:
		return 0x00FF00
	case downRatio <= 0.25:
		return 0xFFFF00
	case downRatio <= 0.5:
		return 0xFFA500
	default:
		return 0xFF0000
	}
}

func sendOrEditDiscordWebhook(payload DiscordWebhookPayload, webhookConfig WebhookConfig) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling JSON: %v", err)
	}

	var req *http.Request
	var actionVerb string

	if webhookConfig.MessageID == "" {
		req, err = http.NewRequest("POST", webhookConfig.URL, bytes.NewBuffer(jsonPayload))
		actionVerb = "sending"
	} else {
		editURL := fmt.Sprintf("%s/messages/%s", webhookConfig.URL, webhookConfig.MessageID)
		req, err = http.NewRequest("PATCH", editURL, bytes.NewBuffer(jsonPayload))
		actionVerb = "editing"
	}

	if err != nil {
		return fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error %s webhook: %v", actionVerb, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func loadWebhookConfigs() ([]WebhookConfig, error) {
	webhookURLs := strings.Split(os.Getenv("DISCORD_WEBHOOK_URLS"), ",")
	messageIDs := strings.Split(os.Getenv("DISCORD_MESSAGE_IDS"), ",")

	if len(webhookURLs) == 1 && webhookURLs[0] == "" {
		return nil, fmt.Errorf("DISCORD_WEBHOOK_URLS environment variable is not set")
	}

	if len(messageIDs) == 1 && messageIDs[0] == "" {
		return nil, fmt.Errorf("DISCORD_MESSAGE_IDS environment variable is not set")
	}

	if len(webhookURLs) != len(messageIDs) {
		return nil, fmt.Errorf("mismatch between number of webhook URLs (%d) and message IDs (%d)", len(webhookURLs), len(messageIDs))
	}

	var configs []WebhookConfig
	for i := range webhookURLs {
		configs = append(configs, WebhookConfig{
			URL:       strings.TrimSpace(webhookURLs[i]),
			MessageID: strings.TrimSpace(messageIDs[i]),
		})
	}

	return configs, nil
}

func main() {
	_ = godotenv.Load()

	webhookConfigs, err := loadWebhookConfigs()
	if err != nil {
		log.Fatalf("Error loading webhook configs: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan []RoomInfo, len(regions))

	for _, region := range regions {
		wg.Add(1)
		go fetchRoomCodes(region, &wg, results)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	allRooms := make(map[string][]RoomInfo)
	for regionRooms := range results {
		if len(regionRooms) > 0 {
			region := regionRooms[0].Region
			allRooms[region] = append(allRooms[region], regionRooms...)
			log.Printf("Processed %d rooms for region %s", len(regionRooms), region)
			for _, room := range regionRooms {
				log.Printf("Room %s in region %s has %d players", room.RoomCode, region, len(room.Players))
			}
		}
	}

	payload := createDiscordPayload(allRooms)
	for _, config := range webhookConfigs {
		err := sendOrEditDiscordWebhook(payload, config)
		if err != nil {
			log.Printf("Error sending/editing Discord webhook (%s): %v", config.URL, err)
		} else {
			if config.MessageID == "" {
				log.Printf("New Discord webhook message sent successfully to %s!", config.URL)
			} else {
				log.Printf("Discord webhook edited successfully for %s!", config.URL)
			}
		}
	}
}
