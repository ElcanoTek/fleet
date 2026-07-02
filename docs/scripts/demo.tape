# vhs tape for the `fleet chat` demo GIF (#540) — see docs/generating-demo-gif.md.
# Runs the REAL TUI against the local mock chat server (mock_chat_server.py):
# no model, no credentials, no cost. Regenerate with:
#   docs/scripts/generate-tui-gif.sh

Output docs/screenshots/tui/demo.gif

Set FontSize 15
Set Width 1080
Set Height 620
Set Padding 12
Set Theme { "background": "#11111B", "foreground": "#E6E6F0", "black": "#16161E", "red": "#FF5C72", "green": "#3DF5A0", "yellow": "#FFC857", "blue": "#9D7CFF", "magenta": "#FF5FD7", "cyan": "#36E2E2", "white": "#E6E6F0", "brightBlack": "#6C7086", "brightRed": "#FF5C72", "brightGreen": "#3DF5A0", "brightYellow": "#FFC857", "brightBlue": "#9D7CFF", "brightMagenta": "#FF5FD7", "brightCyan": "#36E2E2", "brightWhite": "#E6E6F0" }
Set TypingSpeed 35ms

Hide
Type "python3 docs/scripts/mock_chat_server.py 8199 & export FLEET_SERVER_TOKEN=demo-token FLEET_USER_EMAIL=demo@fleet.example; clear"
Enter
Sleep 1s
Type "./fleet chat --server http://127.0.0.1:8199 --model anthropic/claude-opus-4-8"
Enter
Sleep 1.5s
Show

Sleep 1s
Type "How many scheduled tasks failed in the last 24h?"
Sleep 500ms
Enter
# bash + run_python tool calls stream in, then the markdown answer
Sleep 10s

Type "/help"
Sleep 400ms
Enter
Sleep 2.5s

Type "Retry the transient one"
Sleep 500ms
Enter
Sleep 6s

# Hold the finished transcript as the closing shot.
Sleep 3.5s
Hide
Ctrl+D
