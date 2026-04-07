package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"
)

// --- COLOR CONFIGURATION ---
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
)

// --- CONFIGURATION ---
var (
	debugMode bool
)

const APIKeysFile = "apis.conf"

// --- API KEY MANAGER ---
type APIKeyManager struct {
	mu                sync.RWMutex
	Keys              []string
	CurrentIndex      int
	FallbackExhausted bool
}

func NewAPIKeyManager() *APIKeyManager {
	km := &APIKeyManager{
		Keys:              []string{},
		CurrentIndex:      0,
		FallbackExhausted: false,
	}
	km.Load()
	return km
}

func (km *APIKeyManager) Load() {
	km.mu.Lock()
	defer km.mu.Unlock()

	file, err := os.Open(APIKeysFile)
	if err != nil {
		printError("Missing file %s: %v", APIKeysFile, err)
		printWarn("Create %s with one OpenAI API key per line", APIKeysFile)
		os.Exit(1)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key := strings.TrimSpace(scanner.Text())
		if key != "" && !strings.HasPrefix(key, "#") {
			km.Keys = append(km.Keys, key)
		}
	}

	if len(km.Keys) == 0 {
		printError("No API keys found in %s", APIKeysFile)
		os.Exit(1)
	}

	printSuccess("Loaded %d API key(s) from %s", len(km.Keys), APIKeysFile)
}

func (km *APIKeyManager) GetCurrentKey() string {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if km.CurrentIndex >= len(km.Keys) {
		return ""
	}
	return km.Keys[km.CurrentIndex]
}

func (km *APIKeyManager) NextKey() (string, bool) {
	km.mu.Lock()
	defer km.mu.Unlock()

	km.CurrentIndex++

	if km.CurrentIndex >= len(km.Keys) {
		return "", false
	}
	return km.Keys[km.CurrentIndex], true
}

func (km *APIKeyManager) GetCurrentIndex() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.CurrentIndex
}

func (km *APIKeyManager) GetTotalKeys() int {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return len(km.Keys)
}

func (km *APIKeyManager) MarkFallbackExhausted() {
	km.mu.Lock()
	defer km.mu.Unlock()
	km.FallbackExhausted = true
}

func (km *APIKeyManager) IsFallbackExhausted() bool {
	km.mu.RLock()
	defer km.mu.RUnlock()
	return km.FallbackExhausted
}

// --- LOGGING SYSTEM ---
func getTime() string {
	return time.Now().Format("15:04:05.000")
}

func printInfo(format string, v ...interface{}) {
	if debugMode {
		msg := fmt.Sprintf(format, v...)
		fmt.Printf("%s[%s] ℹ️  %s%s\n", ColorCyan, getTime(), msg, ColorReset)
	}
}

func printSuccess(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	fmt.Printf("%s[%s] ✅ %s%s\n", ColorGreen, getTime(), msg, ColorReset)
}

func printWarn(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	fmt.Printf("%s[%s] ⚠️  %s%s\n", ColorYellow, getTime(), msg, ColorReset)
}

func printError(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	fmt.Printf("%s[%s] ❌ %s%s\n", ColorRed, getTime(), msg, ColorReset)
}

func printEvent(format string, v ...interface{}) {
	if debugMode {
		msg := fmt.Sprintf(format, v...)
		fmt.Printf("\n%s---------------------------------------------------%s\n", ColorPurple, ColorReset)
		fmt.Printf("%s[%s] 📩 %s%s\n", ColorPurple, getTime(), msg, ColorReset)
	}
}

// --- SMART MODES CONFIG ---
const ConfigFile = "chat_modes.json"

type ModeManager struct {
	mu    sync.RWMutex
	Modes map[string]string // chatJID -> "off", "auto", "focus"
}

func NewModeManager() *ModeManager {
	mm := &ModeManager{Modes: make(map[string]string)}
	mm.Load()
	return mm
}

func (mm *ModeManager) Load() {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	file, err := os.ReadFile(ConfigFile)
	if err == nil {
		temp := make(map[string]string)
		if err := json.Unmarshal(file, &temp); err == nil {
			mm.Modes = temp
		}
		printSuccess("Loaded modes for %d chats", len(mm.Modes))
	} else {
		printWarn("No modes file found. It will be created automatically.")
	}
}

func (mm *ModeManager) Save() {
	data, _ := json.MarshalIndent(mm.Modes, "", "  ")
	err := os.WriteFile(ConfigFile, data, 0644)
	if err != nil {
		printError("Error saving modes file: %v", err)
	}
}

func (mm *ModeManager) SetMode(chatJID string, mode string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if mm.Modes == nil {
		mm.Modes = make(map[string]string)
	}
	mm.Modes[chatJID] = mode
	mm.Save()
	printInfo("Changed mode for %s to: %s", chatJID, mode)
}

func (mm *ModeManager) GetMode(chatJID string) string {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	if mm.Modes == nil {
		return "off"
	}
	mode, exists := mm.Modes[chatJID]
	if !exists {
		return "off" // Default mode
	}
	return mode
}

// --- AUDIO CACHE FOR REACTIONS ---
const MaxCacheSize = 500 // Store up to 500 recent messages in RAM

type AudioCache struct {
	mu    sync.RWMutex
	items map[string]*events.Message
	order []string // Order for removing the oldest (FIFO)
}

func NewAudioCache() *AudioCache {
	return &AudioCache{
		items: make(map[string]*events.Message),
		order: make([]string, 0, MaxCacheSize),
	}
}

func (ac *AudioCache) Add(msgId string, evt *events.Message) {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	if _, exists := ac.items[msgId]; !exists {
		if len(ac.order) >= MaxCacheSize {
			oldest := ac.order[0]
			ac.order = ac.order[1:]
			delete(ac.items, oldest)
		}
		ac.order = append(ac.order, msgId)
	}
	ac.items[msgId] = evt
}

func (ac *AudioCache) Get(msgId string) *events.Message {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.items[msgId]
}

// --- CLIENT ---
type MyClient struct {
	WAClient   *whatsmeow.Client
	Modes      *ModeManager
	APIKeys    *APIKeyManager
	AudioCache *AudioCache
	Ctx        context.Context
}

func main() {
	flag.BoolVar(&debugMode, "debug", false, "Enable detailed logs")
	flag.Parse()

	os.Stdout.Sync()
	fmt.Printf("%s--- WHATSAPP SMART WHISPER BOT ---%s\n", ColorBlue, ColorReset)

	waLogLevel := "ERROR"
	if debugMode {
		waLogLevel = "INFO"
	}

	dbLog := waLog.Stdout("DB", waLogLevel, true)
	clientLog := waLog.Stdout("WA", waLogLevel, true)

	container, err := sqlstore.New(context.Background(), "sqlite", "file:whatsapp_session.db?_pragma=foreign_keys(1)", dbLog)
	if err != nil {
		log.Fatalf("❌ Database error: %v", err)
	}

	deviceRes, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatalf("❌ Error fetching device: %v", err)
	}

	client := whatsmeow.NewClient(deviceRes, clientLog)
	apiKeys := NewAPIKeyManager()

	myClient := &MyClient{
		WAClient:   client,
		Modes:      NewModeManager(),
		APIKeys:    apiKeys,
		AudioCache: NewAudioCache(),
		Ctx:        context.Background(),
	}
	client.AddEventHandler(myClient.handler)

	if client.Store.ID == nil {
		printWarn("No session found. Generating QR code...")
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			}
		}
	} else {
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	printSuccess("BOT STARTED AND READY!")

	go func() {
		time.Sleep(2 * time.Second)
		if client.IsConnected() {
			myClient.sendToSelf("🤖 *System:* Bot is ready.\n\nSend `!help` in any chat to see available modes.")
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	client.Disconnect()
	printInfo("Shutting down...")
}

func (my *MyClient) handler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		go func(v *events.Message) {
			chatJID := v.Info.Chat.ToNonAD().String()
			senderJID := v.Info.Sender.ToNonAD().String()
			isFromMe := v.Info.IsFromMe

			// Cache the message if it's an audio message
			audioMsg := v.Message.GetAudioMessage()
			if audioMsg != nil {
				my.AudioCache.Add(v.Info.ID, v)
			}

			// 1. MODE MANAGEMENT (COMMANDS)
			txt := strings.TrimSpace(getMsgText(v))
			txtLower := strings.ToLower(txt)

			if txtLower == "!help" || txtLower == "!auto" || txtLower == "!focus" || txtLower == "!off" {
				printEvent("COMMAND from %s: %s", senderJID, txt)

				switch txtLower {
				case "!help":
					msg := "🤖 *Smart Transcription Modes*\n\n" +
						"💤 *!off* (Default) - Ignores everything.\n" +
						"🎯 *!focus* - Transcribes **only** your chat partner (ignores you).\n" +
						"🟢 *!auto* - Listens to and transcribes everyone.\n\n" +
						"💡 *Tip:* While in `!off` mode, react to any voice message with ❓, 📝 or 👀, and I will transcribe it on the fly!"
					my.sendToSelf(msg)

				case "!auto":
					my.Modes.SetMode(chatJID, "auto")
					my.sendToSelf(fmt.Sprintf("🟢 *AUTO mode* enabled (chat: %s).\nTranscribing everything in this chat.", chatJID))

				case "!focus":
					my.Modes.SetMode(chatJID, "focus")
					my.sendToSelf(fmt.Sprintf("🎯 *FOCUS mode* enabled (chat: %s).\nListening only to your partner. Your voice messages will be ignored.", chatJID))

				case "!off":
					my.Modes.SetMode(chatJID, "off")
					my.sendToSelf(fmt.Sprintf("💤 *OFF mode* active (chat: %s).\nDisabled listening in this chat.", chatJID))
				}

				// Automatically remove the command message (if we sent it)
				if isFromMe {
					go func() {
						time.Sleep(300 * time.Millisecond) // Short delay
						revokeMsg := my.WAClient.BuildRevoke(v.Info.Chat, v.Info.Sender, v.Info.ID)
						_, err := my.WAClient.SendMessage(context.Background(), v.Info.Chat, revokeMsg)
						if err != nil {
							printWarn("Failed to delete command: %v", err)
						} else {
							printInfo("Deleted own command: %s", txt)
						}
					}()
				}
				
				return
			}

			// 2. ON-DEMAND REACTIONS (EMOJIS)
			reactMsg := v.Message.GetReactionMessage()
			if reactMsg != nil {
				emoji := reactMsg.GetText()
				// We accept specific emojis: Question mark, Notepad, Eyes
				if emoji == "❓" || emoji == "📝" || emoji == "👀" {
					targetID := reactMsg.GetKey().GetID()
					cachedEvt := my.AudioCache.Get(targetID)
					
					if cachedEvt != nil {
						cachedAudio := cachedEvt.Message.GetAudioMessage()
						if cachedAudio != nil {
							printInfo("On-demand transcription from reaction: %s", emoji)
							
							// Automatically remove reaction (if we added it)
							if isFromMe {
								go func(rKey *proto.MessageKey) {
									delayMs := rand.Intn(1700) + 400 // Random delay 400-2100 ms
									delay := time.Duration(delayMs) * time.Millisecond
									time.Sleep(delay)

									emptyText := ""
									nowTimestamp := time.Now().UnixMilli()

									emptyReact := &proto.Message{
										ReactionMessage: &proto.ReactionMessage{
											Key:               rKey,
											Text:              &emptyText,       // Empty string removes the reaction
											SenderTimestampMS: &nowTimestamp,    
										},
									}

									_, err := my.WAClient.SendMessage(context.Background(), v.Info.Chat, emptyReact)
									if err != nil {
										printWarn("Failed to remove reaction: %v", err)
									} else {
										printInfo("Removed own reaction '%s' after %v", emoji, delay)
									}
								}(reactMsg.GetKey())
							}

							// Use cachedEvt to make the bot reply as a quote to the original audio message
							my.processAudioMessage(cachedEvt, cachedAudio, true)
						}
					} else {
						printWarn("Ignored reaction %s: audio not saved in RAM cache (e.g., from before bot restart).", emoji)
					}
				}
				return
			}

			// 3. REGULAR VOICE MESSAGE HANDLING
			if audioMsg != nil {
				mode := my.Modes.GetMode(chatJID)

				if mode == "off" {
					printInfo("Chat %s is in 'off' mode. Ignoring audio.", chatJID)
					return
				}

				if mode == "focus" && isFromMe {
					printInfo("Chat %s is in 'focus' mode. Ignoring own message.", chatJID)
					return
				}

				// If mode is "auto", or ("focus" and message is NOT from us) -> transcribe
				my.processAudioMessage(v, audioMsg, false)
			}
		}(v)
	}
}

// processAudioMessage downloads, transcribes, and sends the response
func (my *MyClient) processAudioMessage(v *events.Message, audioMsg *proto.AudioMessage, isForced bool) {
	printInfo("Downloading audio message...")
	data, err := my.WAClient.Download(context.Background(), audioMsg)
	if err != nil {
		printError("Download error: %v", err)
		return
	}

	startTranscribe := time.Now()
	transcription, err := my.transcribeWithRotation(data)

	if err != nil {
		printError("TRANSCRIPTION ERROR: %v", err)
		my.sendToSelf(fmt.Sprintf("⚠️ Transcription error in chat:\n%v", err))
		return
	}

	if strings.TrimSpace(transcription) == "" {
		transcription = "[No speech detected / Silence]"
	}

	duration := time.Since(startTranscribe)
	printSuccess("Transcription ready in %v", duration)
	
	prefix := ""
	if isForced {
		prefix = "> " // Add a small prefix to know it was triggered by a reaction
	}

	my.reply(v, fmt.Sprintf("%s```%s```", prefix, transcription))
}

func getMsgText(v *events.Message) string {
	if v.Message.Conversation != nil {
		return *v.Message.Conversation
	}
	if v.Message.ExtendedTextMessage != nil && v.Message.ExtendedTextMessage.Text != nil {
		return *v.Message.ExtendedTextMessage.Text
	}
	return ""
}

// transcribeWithRotation handles OpenAI requests with API key rotation.
func (my *MyClient) transcribeWithRotation(audioData []byte) (string, error) {
	for {
		apiKey := my.APIKeys.GetCurrentKey()
		if apiKey == "" {
			return "", fmt.Errorf("no API keys available")
		}

		res, err := my.transcribeAudio(audioData, apiKey)
		if err == nil {
			return res, nil
		}

		errMsg := err.Error()
		isQuotaError := strings.Contains(errMsg, "429") ||
			strings.Contains(errMsg, "quota") ||
			strings.Contains(errMsg, "insufficient_quota") ||
			strings.Contains(errMsg, "Rate limit")

		if isQuotaError && !my.APIKeys.IsFallbackExhausted() {
			printWarn("OpenAI API limit exhausted on current key!")

			_, hasMore := my.APIKeys.NextKey()
			if hasMore {
				printWarn("Switching to the next key (%d/%d)...",
					my.APIKeys.GetCurrentIndex()+1,
					my.APIKeys.GetTotalKeys())

				my.sendToSelf(fmt.Sprintf("🔄 *API Key Rotation*\nPrevious key exhausted.\nNew key: %d/%d",
					my.APIKeys.GetCurrentIndex()+1,
					my.APIKeys.GetTotalKeys()))
				continue
			} else {
				my.APIKeys.MarkFallbackExhausted()
				printWarn("All API keys exhausted!")
				my.sendToSelf("⚠️ *System Alert*\nAll OpenAI API keys have been exhausted.")
			}
		}

		return "", err
	}
}

// transcribeAudio performs a clean HTTP POST request to OpenAI
func (my *MyClient) transcribeAudio(audioData []byte, apiKey string) (string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	
	_ = writer.WriteField("model", "whisper-1")
	_ = writer.WriteField("response_format", "text")

	part, err := writer.CreateFormFile("file", "ptt.oga")
	if err != nil {
		return "", fmt.Errorf("error creating form file: %v", err)
	}

	_, err = part.Write(audioData)
	if err != nil {
		return "", fmt.Errorf("error writing file: %v", err)
	}

	err = writer.Close()
	if err != nil {
		return "", fmt.Errorf("error closing form: %v", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", body)
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	responseText := string(responseBody)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error (%d): %s", resp.StatusCode, responseText)
	}

	return responseText, nil
}

func (my *MyClient) reply(v *events.Message, text string) {
	participantJID := v.Info.Sender.ToNonAD().String()

	my.WAClient.SendMessage(context.Background(), v.Info.Chat, &proto.Message{
		ExtendedTextMessage: &proto.ExtendedTextMessage{
			Text: &text,
			ContextInfo: &proto.ContextInfo{
				StanzaID:      &v.Info.ID,
				Participant:   &participantJID,
				QuotedMessage: v.Message,
			},
		},
	})
}

func (my *MyClient) sendToSelf(text string) {
	myJID := my.WAClient.Store.ID.ToNonAD()
	_, err := my.WAClient.SendMessage(context.Background(), myJID, &proto.Message{
		ExtendedTextMessage: &proto.ExtendedTextMessage{
			Text: &text,
		},
	})
	if err != nil {
		printError("Error sending to self: %v", err)
	}
}
