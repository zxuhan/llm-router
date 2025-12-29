# vhs tape: real live demo against a running router (must boot workers
# and the router first; the script does NOT start them itself, so the
# recording stays clean and short).
#
# Render: vhs bench/scripts/demo.tape  -> docs/demo.gif

Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 580
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 18ms

Hide
Type "cd $(git rev-parse --show-toplevel) && clear"
Enter
Sleep 200ms
Show

Type "bash bench/scripts/live-demo.sh"
Sleep 500ms
Enter
Sleep 12s
