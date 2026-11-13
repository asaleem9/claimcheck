# Records docs/whatif.gif. Needs the kind cluster from hack/kind/up.sh with
# docs/demo/setup.yaml and docs/demo/llm-train.yaml applied:
#   make build && kubectl apply -f docs/demo/setup.yaml -f docs/demo/llm-train.yaml && vhs docs/whatif.tape
Output docs/whatif.gif

Set Shell "bash"
Set FontSize 13
Set Width 1440
Set Height 620
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms

Hide
Type "export PATH=$PWD/bin:$PATH PS1='\[\e[1;36m\]$ \[\e[0m\]'; clear"
Enter
Sleep 1s
Show

Type "# which single change would let llm-train fit?"
Enter
Type "kubectl claimcheck whatif pod/llm-train -n ml --suggest"
Enter
Sleep 4s
Type "kubectl claimcheck pools"
Enter
Sleep 7s
