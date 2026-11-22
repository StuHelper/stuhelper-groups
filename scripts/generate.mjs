import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

const check = process.argv.includes('--check');
const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'stuhelper-groups-generate-'));
try {
  fs.mkdirSync('sdk', { recursive: true });
  const goOutput = check ? path.join(temporary, 'groups.gen.go') : 'sdk/groups.gen.go';
  const tsOutput = check ? path.join(temporary, 'groups.gen.ts') : 'sdk/groups.gen.ts';
  // Keep the published Go interface method sets so existing clients and mocks still compile.
  // sdk/deliveries.go exposes the corrected spelling on the concrete clients.
  const goContract = JSON.parse(fs.readFileSync('contracts/groups.openapi.json', 'utf8'));
  const deliveries = goContract.paths['/api/v1/groups/{group}/deliveries'].get;
  assert.equal(deliveries.operationId, 'listDeliveries');
  deliveries.operationId = 'listDeliverys';
  const goInput = path.join(temporary, 'groups-go.openapi.json');
  fs.writeFileSync(goInput, JSON.stringify(goContract));
  execFileSync('go', ['run', 'github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0', '-generate', 'types,client', '-package', 'sdk', '-o', goOutput, goInput], { stdio: 'inherit' });
  execFileSync('pnpm', ['exec', 'openapi-typescript', 'contracts/groups.openapi.json', '-o', tsOutput], { stdio: 'inherit' });
  if (check) {
    for (const [expected, actual] of [['sdk/groups.gen.go', goOutput], ['sdk/groups.gen.ts', tsOutput]]) {
      assert.equal(fs.readFileSync(expected, 'utf8'), fs.readFileSync(actual, 'utf8'), `Generated contract differs: ${expected}`);
    }
  }
} finally {
  fs.rmSync(temporary, { recursive: true, force: true });
}
