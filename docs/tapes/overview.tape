Output ./docs/assets/overview.gif

Set Shell "zsh"
Set FontSize 16
Set Width 600
Set Height 400
Set TypingSpeed 80ms
Set Padding 20

# Throwaway config so --save neither touches the recorder's real aliases nor
# shows a personal path in the gif.
Env XDG_CONFIG_HOME "/tmp/eta-demo"

Type "eta oslo 2 holmen -c 3 -t --save home"
Sleep 0.3s
Enter
Sleep 5s

Type "eta home"
Sleep 0.3s
Enter
Sleep 5s
