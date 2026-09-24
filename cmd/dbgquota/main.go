package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"codebuddy2api/internal/cred"
	"codebuddy2api/internal/upstream"
)

func main() {
	uid := os.Args[1]
	raw, err := os.ReadFile("/opt/codebuddy2api/auths/codebuddy-" + uid + ".json")
	if err != nil {
		panic(err)
	}
	var c cred.Cred
	if err := json.Unmarshal(raw, &c); err != nil {
		panic(err)
	}
	cl := upstream.New()
	info, err := cl.FetchQuota(context.Background(), "https://copilot.tencent.com", c.Token, nil)
	if err != nil {
		fmt.Println("FetchQuota err:", err)
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(b))
}
