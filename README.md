    可以替代tailscale ssh, 利用ssh proxycommand, 在~/.bashrc里添加, 并souce ~/.bashrc:
    ssh-p() {
      if [[ $# -lt 2 ]]; then
          echo "Usage: ssh-p <port> [ssh options] user@host"
          return 1
      fi
    
      local port="$1"
      shift
    
      ssh -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ProxyCommand="nc -X 5 -x 100.71.153.100:$port %h %p" "$@"
    }
    然后就可以 ssh-p 3211 root@127.0.0.1 来访问目标代理的机器本身

# SSHDog

SSHDog is your go-anywhere lightweight SSH server.  Written in Go, it aims
to be a portable SSH server that you can drop on a system and use for remote
access without any additional configuration.

Useful for:

* Tech support
* Backup SSHD
* Authenticated remote bind shells

Supported features:

* Windows & Linux
* Configure port, host key, authorized keys
* Pubkey authentication (no passwords)
* Port forwarding
* SCP (but no SFTP support)

Example usage:

```
% echo 2222 > config/port
% cp ~/.ssh/id_rsa.pub config/authorized_keys
% go build -ldflags="-s -w" .
% mv sshdog my_sshdog        #because the sshdog contains your pub key, so it's only for you
% ./my_sshdog
[DEBUG] Generating random host key...
[DEBUG] Adding authorized_keys.
[DEBUG] Listening on :2222
[DEBUG] Waiting for shutdown.
[DEBUG] select...
```

## Security

- **Host key**: Generated randomly at runtime. This prevents private key leakage if the binary is compromised. Note that clients will see a host key change warning on each restart.
- **authorized_keys**: Embedded at build time for convenience.

To connect without host key verification (useful for serverless/ephemeral environments):

```
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 2222 any@your_machine_ip
```

Note: Configuration files in the `config/` directory are embedded into the binary at build time using Go's native `embed` package. No additional tools are required.

Author: David Tomaschik <dwt@google.com>

*This is not a Google product, merely code that happens to be owned by Google.*



