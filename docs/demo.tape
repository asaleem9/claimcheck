# Records docs/demo.gif. Needs the kind cluster from hack/kind/up.sh with
# docs/demo/setup.yaml applied, and bin/ on PATH:
#   make build && kubectl apply -f docs/demo/setup.yaml && vhs docs/demo.tape
Output docs/demo.gif

Set Shell "bash"
Set FontSize 13
Set Width 1440
Set Height 560
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms

Hide
Type "export PATH=$PWD/bin:$PATH PS1='\[\e[1;36m\]$ \[\e[0m\]' && kubectl delete -f docs/demo/llm-train.yaml --ignore-not-found --wait >/dev/null 2>&1; clear"
Enter
Sleep 2s
Show

Type "kubectl apply -f docs/demo/llm-train.yaml"
Enter
Sleep 3s
Type "kubectl get pod llm-train -n ml -o jsonpath='{.status.conditions[0].message}'; echo"
Enter
Sleep 3500ms
Type "# ...but why? "
Sleep 800ms
Enter
Type "kubectl claimcheck explain pod/llm-train -n ml"
Enter
Sleep 9s
