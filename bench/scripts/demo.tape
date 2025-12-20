# vhs tape: demo of the bench summary report. The report is pre-cooked
# in docs/demo-output.txt so the gif is self-contained (the real bench
# takes ~15 min; we don't want to actually run it inside the recording).
# Render: vhs bench/scripts/demo.tape  -> docs/demo.gif

Output docs/demo.gif

Set Shell "bash"
Set FontSize 14
Set Width 1100
Set Height 580
Set Padding 22
Set Theme "Catppuccin Mocha"
Set TypingSpeed 12ms

Hide
Type "cd $(git rev-parse --show-toplevel) && clear"
Enter
Sleep 200ms
Show

Type "cat docs/demo-output.txt"
Sleep 600ms
Enter
Sleep 8s
