package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TASK-4041, DOC-4036: this independent declaration is socket-scoped only.
// Reuse the owner stamps without changing the push arm file's JSON contract.
func chatDirectionPath() (string, error) {
	socket := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	if socket == "" {
		return "", fmt.Errorf("chat direction requires a session messaging socket")
	}
	dir, err := SessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "chat-direction-dir-"+shortHash(socket)+".json"), nil
}

func WriteChatDirection() (string, error) {
	path, err := chatDirectionPath()
	if err != nil {
		return "", err
	}
	socket := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	info, err := os.Stat(socket)
	if err != nil {
		return "", fmt.Errorf("chat session socket: %w", err)
	}
	cwd, _ := os.Getwd()
	st := ArmState{Armed: true, PID: os.Getpid(), Socket: socket, Cwd: cwd, StartedAt: time.Now().UTC().Format(time.RFC3339), SocketMtimeUnixNano: info.ModTime().UnixNano()}
	if ino, dev, ok := statIdentity(info); ok {
		st.SocketIno, st.SocketDev = ino, dev
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

func readChatDirection(path string) (*ArmState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st ArmState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ChatDirectionOn reads fresh on every call and requires explicit delivery consent.
func ChatDirectionOn() bool {
	path, err := chatDirectionPath()
	if err != nil {
		return false
	}
	st, err := readChatDirection(path)
	if err != nil || !armStateWellFormed(st) || !st.Armed || st.Socket != os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") {
		return false
	}
	switch armStateOwnerVerdict(st) {
	case LivenessDead:
		reapOwnedArmFile(path, func() (*ArmState, error) { return readChatDirection(path) })
		return false
	case LivenessAlive:
		return SessionArmState() == LocalArmOn
	default:
		return false
	}
}

func ClearChatDirection() error {
	path, err := chatDirectionPath()
	if err != nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func VerifyChatDirectionHeld() error {
	if !ChatDirectionOn() {
		return fmt.Errorf("chat direction did not take; run pad session arm for delivery and retry in a live messaging session")
	}
	return nil
}
