# Reproducible local PoW network

`network.sh` manages an isolated multi-node PFAP network. It creates one
account per node, initializes every datadir from the same genesis, starts the
nodes, and connects them in a full-mesh topology. Node 1 mines by
default. The checked-in test genesis uses Ethash's minimum difficulty so local
blocks are produced quickly; it must never be used as a production network.

```bash
cp network.env.example network.env
./network.sh start
./network.sh status
./network.sh smoke
./network.sh attach 1
./network.sh stop
```

Change `NODE_COUNT`, port bases, the geth path, and mining/HTTP settings in
`network.env`. Runtime state stays under `.network/` and is not committed.
HTTP RPC is disabled by default; when enabled it binds only to `127.0.0.1`.
When run from a source checkout, `PFAP_PRFKEY_DIR` automatically points to the
checkout's `prfKey/`; installed bundles default to `/usr/local/prfKey`.

For isolated CI environments that prohibit TCP listeners, a single node can
run without P2P networking:

```bash
NODE_COUNT=1 OFFLINE=true ./network.sh start
```

Offline mode passes `--port -1`, a PFAP extension that disables the P2P TCP
listener. IPC remains enabled so the normal `attach` commands still work. In a
sandbox that also prohibits Unix sockets, use `--ipcdisable console` and run
the same JavaScript transaction commands directly in the foreground console.

Useful non-interactive checks:

```bash
./network.sh attach 1 'admin.peers.length'
./network.sh attach 1 'eth.blockNumber'
./network.sh attach 2 'eth.getAccountState()'
```

`./network.sh clean` stops all managed nodes and removes only the configured
runtime directory beneath this test directory.

## Multiple machines

After a full build, create a reproducible runtime bundle:

```bash
./build.sh bundle
```

Copy `dist/pfap-runtime.tar.gz` to each host, verify the adjacent SHA-256 file,
extract it, and run `sudo ./pfap-runtime/scripts/install-runtime.sh`. Each host
then has the same geth binary, native libraries, proof keys, genesis, and
network tooling. For cross-host tests, set distinct port bases and replace the
local automatic peering step with the remote node's advertised enode address.
