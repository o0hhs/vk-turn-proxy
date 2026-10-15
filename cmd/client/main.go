// SPDX-License-Identifier: MIT
//
// The command line front end. It is the upstream client's main() in a separate binary: same
// flags, same output, same signal handling — so the client stays usable by hand, and so the
// library behind it can be diffed against upstream by running both.

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	vkclient "github.com/cacggghp/vk-turn-proxy/client"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signalChan
		log.Printf("Terminating...\n")
		cancel()
		select {
		case <-signalChan:
		case <-time.After(5 * time.Second):
		}
		log.Fatalf("Exit...\n")
	}()

	host := flag.String("turn", "", "override TURN server ip")
	port := flag.String("port", "", "override TURN port")
	listen := flag.String("listen", "127.0.0.1:9000", "listen on ip:port")
	vklink := flag.String("vk-link", "", "VK calls invite link \"https://vk.com/call/join/...\"")
	yalink := flag.String("yandex-link", "", "Yandex telemost invite link \"https://telemost.yandex.ru/j/...\"")
	peerAddr := flag.String("peer", "", "peer server address (host:port)")
	n := flag.Int("n", 0, "connections to TURN (default 10 for VK, 1 for Yandex)")
	udp := flag.Bool("udp", false, "connect to TURN with UDP")
	direct := flag.Bool("no-dtls", false, "connect without obfuscation. DO NOT USE")
	vlessMode := flag.Bool("vless", false, "VLESS mode: forward TCP connections (for VLESS) instead of UDP packets")
	debugFlag := flag.Bool("debug", false, "enable debug logging")
	manualCaptchaFlag := flag.Bool("manual-captcha", false, "skip auto captcha solving, use manual mode immediately")
	flag.Parse()

	if *peerAddr == "" {
		log.Panicf("Need peer address!")
	}
	if (*vklink == "") == (*yalink == "") {
		log.Panicf("Need either vk-link or yandex-link!")
	}
	if *direct {
		// Kept from upstream: the flag exists, the mode does not.
		log.Panicf("Direct mode not supported with dispatcher")
	}

	cfg := vkclient.Config{
		Peer:          *peerAddr,
		Listen:        *listen,
		VkLink:        *vklink,
		YandexLink:    *yalink,
		NumStreams:    *n,
		UDP:           *udp,
		VLESS:         *vlessMode,
		Debug:         *debugFlag,
		ManualCaptcha: *manualCaptchaFlag,
		TurnHost:      *host,
		TurnPort:      *port,
	}

	if err := vkclient.Run(ctx, cfg); err != nil {
		log.Panicf("%s", err)
	}
}
