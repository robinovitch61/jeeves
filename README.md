# jeeves

<p>
    <a href="https://github.com/robinovitch61/jeeves/releases"><img src="https://shields.io/github/v/release/robinovitch61/jeeves.svg" alt="Latest Release"></a>
    <a href="https://pkg.go.dev/github.com/robinovitch61/jeeves?tab=doc"><img src="https://godoc.org/github.com/golang/gddo?status.svg" alt="GoDoc"></a>
    <a href="https://github.com/robinovitch61/jeeves/actions"><img src="https://github.com/robinovitch61/jeeves/workflows/build/badge.svg" alt="Build Status"></a>
</p>

A TUI for browsing and resuming AI agent sessions.

<img src="./demo/demo.gif" alt="gif demo of jeeves"/>

* Browse all your AI agent sessions in one place
    * Claude Code and Codex supported
* Search sessions by content
* Preview conversation contents in a split pane
* Open conversations to read the full conversation
* Resume a session directly in the agent

## Usage

[Install](#installation) and run `jeeves` or `jeeves <some search term>` in a terminal.

```shell
# Browse all sessions
jeeves

# Search for sessions containing "ui refactor"
jeeves ui refactor

# Search with a regex pattern
jeeves "fix.*bug"
```

| Key       | Action                    |
|-----------|---------------------------|
| enter     | open session fullscreen   |
| r         | resume session in agent   |
| /         | filter sessions           |
| j/k       | navigate up/down          |
| w         | toggle line wrap          |
| esc       | back to list              |
| q/ctrl+c  | quit                      |

## Installation

```shell
# homebrew
brew install robinovitch61/tap/jeeves

# upgrade using homebrew
brew update && brew upgrade jeeves

# nix-shell
# ensure NUR is accessible (https://github.com/nix-community/NUR)
nix-shell -p nur.repos.robinovitch61.jeeves

# nix flakes
# ensure flake support is enabled (https://nixos.wiki/wiki/Flakes#Enable_flakes_temporarily)
nix run github:robinovitch61/nur-packages#jeeves

# arch linux
# PKGBUILD available at https://aur.archlinux.org/packages/jeeves-bin
yay -S jeeves-bin

# with go (https://go.dev/doc/install)
go install github.com/robinovitch61/jeeves@latest

# windows with winget
winget install robinovitch61.jeeves

# windows with scoop
scoop bucket add robinovitch61 https://github.com/robinovitch61/scoop-bucket
scoop install jeeves

# windows with chocolatey
choco install jeeves
```

You can also download [prebuilt releases](https://github.com/robinovitch61/jeeves/releases) and move the unpacked
binary to somewhere in your `PATH`.

## Development

`jeeves` is written with tools from [Charm](https://charm.sh/) and relies heavily on the [robinovitch61 viewport bubble](https://github.com/robinovitch61/viewport).

[Feature requests and bug reports are welcome](https://github.com/robinovitch61/jeeves/issues/new/choose).

To manually build the project:

```shell
git clone git@github.com:robinovitch61/jeeves.git
cd jeeves
go build  # outputs ./jeeves executable
```
