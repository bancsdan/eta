Output ./docs/assets/overview.gif

Set Shell "zsh"
Set FontSize 16
Set Width 1000
Set Height 760
Set TypingSpeed 60ms
Set Padding 20

# Throwaway config so --save neither touches the recorder's real aliases nor
# shows a personal path in the gif.
Env XDG_CONFIG_HOME "/tmp/eta-demo"

Type "eta countries"
Sleep 0.3s
Enter
Sleep 4s

Type "clear"
Sleep 0.3s
Enter
Sleep 0.5s

Type "eta norway oslo 31 jernbanetorget -c 2 -t"
Sleep 0.3s
Enter
Sleep 4s

Type "eta norway halden bussterminal -c 1"
Sleep 0.3s
Enter
Sleep 5s

Type "clear"
Sleep 0.3s
Enter
Sleep 0.5s

Type "eta uk london Central -l | head -8"
Sleep 0.3s
Enter
Sleep 4s

Type 'eta usa boston Red "park street" --save home'
Sleep 0.3s
Enter
Sleep 4s

Type "eta home -c 2"
Sleep 0.3s
Enter
Sleep 5s

