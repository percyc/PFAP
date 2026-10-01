function requireStoppedProcessAudit(report, experimentId, serverIds) {
  const expected = new Set(serverIds);
  if (expected.size !== 100 || report?.experimentId !== experimentId || report.readOnly !== true ||
      !Array.isArray(report.results) || report.results.length !== expected.size) {
    throw Error('Process audit scope differs from the 100-server experiment');
  }
  const seen = new Set();
  for (const row of report.results) {
    if (!expected.has(row.serverId) || seen.has(row.serverId)) throw Error('Missing, duplicate or foreign audit server');
    seen.add(row.serverId);
    if (row.error !== null || !Array.isArray(row.processes) || row.processes.length ||
        !Number.isFinite(row.memoryKiB?.MemTotal) || row.memoryKiB.MemTotal <= 0) {
      throw Error('Residual or unverified geth processes; do not start the next group');
    }
  }
}
module.exports = {requireStoppedProcessAudit};
