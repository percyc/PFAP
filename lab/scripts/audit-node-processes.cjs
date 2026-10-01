// Read-only inventory of geth processes on one experiment's servers.
// Never stops processes; unknown ownership must be reviewed separately.
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const root = path.resolve(__dirname, '../..');
const [experimentId, output] = process.argv.slice(2);
if (!/^exp-[a-f0-9]+$/.test(experimentId || '') || !output) throw Error('Usage: audit-node-processes.cjs EXPERIMENT_ID NEW_REPORT.json');
const state = JSON.parse(fs.readFileSync(path.join(root, 'lab/data/lab.json')));
const experiment = state.experiments.find(e => e.id === experimentId);
if (!experiment) throw Error('Unknown experiment');
const ids = new Set(experiment.nodes.map(n => n.serverId));
const servers = state.servers.filter(s => ids.has(s.id));
if (servers.length !== ids.size) throw Error('Missing server configuration');
if (servers.some(s => s.host === '192.168.50.13')) throw Error('Important physical host excluded');
const command = `ps -C geth -o pid=,rss=,args=
audit_status=$?
if [ "$audit_status" -gt 1 ]; then exit "$audit_status"; fi
for audit_pid in $(ps -C geth -o pid=); do
  if [ -r /proc/$audit_pid/status ]; then
    awk -v pid="$audit_pid" '/^(VmHWM|RssAnon|RssFile|Threads):/ {print "PFAP_PROC", pid, $1, $2}' /proc/$audit_pid/status 2>/dev/null
  fi
done
cat /proc/meminfo`;
function inspect(server) {
  const args = server.host === 'local' ? ['-c', command] : [
    '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=8',
    ...(server.knownHostsFile ? ['-o', 'UserKnownHostsFile=' + server.knownHostsFile] : []),
    ...(server.identityFile ? ['-i', server.identityFile] : []),
    '-p', String(server.port || 22), `${server.user}@${server.host}`, command,
  ];
  return new Promise(resolve => {
    let stdout = '', done = false;
    const child = spawn(server.host === 'local' ? 'bash' : 'ssh', args, {stdio: ['ignore', 'pipe', 'pipe']});
    const finish = error => {
      if (done) return;
      done = true; clearTimeout(timer);
      const processes = [], memoryKiB = {}, processMemory = new Map();
      for (const line of stdout.split('\n')) {
        const stat = line.match(/^PFAP_PROC (\d+) (VmHWM|RssAnon|RssFile|Threads): (\d+)$/);
        if (stat) {
          const fields = {VmHWM: 'peakRssKiB', RssAnon: 'anonRssKiB', RssFile: 'fileRssKiB', Threads: 'threads'};
          const values = processMemory.get(Number(stat[1])) || {};
          values[fields[stat[2]]] = Number(stat[3]);
          processMemory.set(Number(stat[1]), values);
        }
        const m = line.match(/^(MemTotal|MemAvailable|SwapTotal|SwapFree):\s+(\d+)/);
        if (m) memoryKiB[m[1]] = Number(m[2]);
        const p = line.match(/^\s*(\d+)\s+(\d+)\s+(.+)$/);
        if (!p) continue;
        const datadir = p[3].match(/(?:^|\s)--datadir(?:=|\s+)(\S+)/)?.[1] || '';
        const owner = state.experiments.find(e => e.nodes.some(n => n.serverId === server.id &&
          datadir === path.posix.join(server.workDir, 'experiments', e.id, server.id, 'node' + n.localIndex)));
        processes.push({pid: Number(p[1]), rssKiB: Number(p[2]), datadir,
          experimentId: owner?.id || null, experimentStatus: owner?.status || null,
          classification: owner?.id === experimentId ? 'target-experiment' : owner ? 'other-recorded-experiment' : 'unrecognized'});
      }
      // VmHWM belongs to this process lifetime, not to one measurement window.
      // A short-lived attach helper may disappear between ps and /proc reads.
      for (const process of processes) Object.assign(process, processMemory.get(process.pid));
      if (!error && !memoryKiB.MemTotal) error = 'Incomplete memory response';
      resolve({serverId: server.id, host: server.host, hostGroup: server.hostGroup,
        checkedAt: new Date().toISOString(), error: error || null, memoryKiB, processes});
    };
    const timer = setTimeout(() => {child.kill('SIGTERM'); finish('Inspection timed out');}, 20000);
    child.stdout.on('data', b => {stdout += b; if (stdout.length > 1048576) {child.kill('SIGTERM'); finish('Output limit exceeded');}});
    child.stderr.on('data', () => {}); // Do not expose SSH configuration in reports.
    child.on('error', e => finish(e.code || 'Spawn error'));
    child.on('close', code => finish(code === 0 ? null : 'Inspection exit ' + code));
  });
}
(async () => {
  const results = []; let cursor = 0;
  await Promise.all(Array.from({length: Math.min(5, servers.length)}, async () => {
    while (cursor < servers.length) {
      const result = await inspect(servers[cursor++]); results.push(result);
      console.log(JSON.stringify({serverId: result.serverId, error: result.error, processes: result.processes.length,
        unexpected: result.processes.filter(p => p.classification !== 'target-experiment')}));
    }
  }));
  results.sort((a,b) => a.serverId.localeCompare(b.serverId));
  const report = {experimentId, completedAt: new Date().toISOString(), readOnly: true, results};
  fs.writeFileSync(path.resolve(output), JSON.stringify(report, null, 2), {flag: 'wx', mode: 0o600});
  console.log(JSON.stringify({servers: results.length, failures: results.filter(r => r.error).length,
    processes: results.reduce((n,r) => n + r.processes.length, 0), report: path.resolve(output)}));
  if (results.some(r => r.error)) process.exitCode = 1;
})().catch(e => {console.error(e.message); process.exitCode = 1;});
