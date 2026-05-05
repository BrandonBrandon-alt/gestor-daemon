// Package sshutil provides SSH connection and file transfer utilities.
package sshutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gestor-daemon/internal/config"
	scp "github.com/bramvdbogaerde/go-scp"
	"golang.org/x/crypto/ssh"
)

var sshKeyPath = getSSHKeyPath()

// getSSHKeyPath retrieves the path to the SSH private key file.
func getSSHKeyPath() string {
	if val := os.Getenv("SSH_KEY_PATH"); val != "" {
		return val
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".vagrant.d", "insecure_private_key")
}

// BuildSSHConfig constructs an SSH client configuration for remote command execution.
func BuildSSHConfig() (*ssh.ClientConfig, error) {
	authMethods := []ssh.AuthMethod{
		ssh.Password(config.Global.SSHUser),
	}

	if key, err := os.ReadFile(sshKeyPath); err == nil {
		if signer, err := ssh.ParsePrivateKey(key); err == nil {
			authMethods = append([]ssh.AuthMethod{ssh.PublicKeys(signer)}, authMethods...)
		}
	}

	return &ssh.ClientConfig{
		User:            config.Global.SSHUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

// SSHClient establishes an SSH connection to a remote machine.
func SSHClient(ip string) (*ssh.Client, error) {
	cfg, err := BuildSSHConfig()
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", ip+":22", cfg)
}

// RunSSH executes a command on a remote machine via SSH.
func RunSSH(ip, command string) (string, error) {
	client, err := SSHClient(ip)
	if err != nil {
		return "", err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

// UploadFile transfers a file from the local machine to a remote machine using SCP.
func UploadFile(ip, localPath, remotePath string) error {
	cfg, err := BuildSSHConfig()
	if err != nil {
		return err
	}
	scpClient := scp.NewClient(ip+":22", cfg)
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("error abriendo archivo local: %v", err)
	}
	defer file.Close()
	if err = scpClient.Connect(); err != nil {
		return fmt.Errorf("error conectando SCP: %v", err)
	}
	defer scpClient.Close()
	if err = scpClient.CopyFromFile(context.Background(), *file, remotePath, "0755"); err != nil {
		return fmt.Errorf("error copiando archivo: %v", err)
	}
	return nil
}

// CreateRemoteFile creates or overwrites a file on a remote machine via SSH.
func CreateRemoteFile(ip, remotePath, content string) error {
	tmpFile, err := os.CreateTemp("", "service-*.service")
	if err != nil {
		return fmt.Errorf("error creando archivo temporal: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("error escribiendo archivo temporal: %v", err)
	}
	tmpFile.Close()
	remoteTmp := "/tmp/daemon-service-temp.service"
	if err := UploadFile(ip, tmpFile.Name(), remoteTmp); err != nil {
		return fmt.Errorf("error subiendo .service: %v", err)
	}
	_, err = RunSSH(ip, fmt.Sprintf("sudo mv %s %s && sudo chmod 644 %s", remoteTmp, remotePath, remotePath))
	return err
}
