# whatsmeow-transcribe (Smart Whisper Edition)
This is a heavily modified and extended version of the project originally found at:
https://github.com/hoehermann/whatsmeow-transcribe/
This bot connects to WhatsApp and uses the OpenAI Whisper API to automatically transcribe voice messages. It features a stealthy mode system, automatic API key rotation, and on-demand transcriptions via message reactions.
## Features
 * **OpenAI Whisper Integration:** Directly interacts with the Whisper API for highly accurate audio transcription.
 * **API Key Rotation:** Provide multiple OpenAI keys in apis.conf. If one runs out of quota, the bot seamlessly rotates to the next one without dropping messages.
 * **Stealth Operations:** All command confirmations and system alerts are sent privately to your "Saved Messages" (chat with yourself). The bot also instantly deletes your command messages so your chat partners remain unaware.
 * **Per-Chat Smart Modes:**
   * !off (Default): The bot sleeps. It ignores all voice messages to prevent spam.
   * !focus: The bot only transcribes the voice messages of your chat partner. It ignores yours.
   * !auto: The bot transcribes everything sent in the chat.
 * **Reaction-Based Transcription:** Even when the chat is in !off mode, you can react to any recently received voice message with a specific emoji (question mark, notepad, or eyes). The bot will detect the reaction, transcribe the audio, and then automatically delete your reaction (with a randomized delay to look natural).
## Setup
 1. Clone this repository.
 2. Create a file named apis.conf in the project root.
 3. Add your OpenAI API keys to apis.conf (one key per line).
 4. Run the application:
   go run main.go
 5. Scan the QR code displayed in the terminal using your WhatsApp mobile app (Linked Devices).
## Usage
Once running, type !help in any WhatsApp chat. The bot will delete your message and send instructions to your private "Saved Messages".
Use the following commands in any chat to change the bot's behavior for that specific conversation:
 * !off
 * !focus
 * !auto
All configurations are automatically saved to chat_modes.json.

