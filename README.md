# clipboard-sync
A simple clipbarod sync tool for virtualbox guest.

## The story
There's clipboard sync feature inside virtualbox, which sounds great. BUT, the sync failed sometimes,
for example it failed when doing copy in google chrome. So I wrote up a simple program for doing 
the sync between virtual box client and your host. 

## Usage
## 0 Prepare
### A Prepare dependencies
```
go get -u "github.com/atotto/clipboard"
go get -u  "github.com/op/go-logging"
go get -u "github.com/pingcap/errors"
```
## 1 Build
It's simple, see build.sh for details.

## 2 Host side(server, your desktop)
Run clipboard-sync.server from command line.

## 3 Client side(client, your guest desktop)
Run clipboard-sync.client.exe


## Note
For both server and client, you need to have xclip installed. 