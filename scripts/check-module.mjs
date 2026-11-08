import fs from 'node:fs';
import YAML from 'yaml';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
const files = fs.readdirSync('.', {recursive:true}).filter(f => f.endsWith('.go') && !f.startsWith('node_modules/') && !f.startsWith('.tools/'));
for(const f of files) { const s=fs.readFileSync(f,'utf8'); if (!f.startsWith('postgres/') && !f.endsWith('_test.go')) assert(!/\b(?:SELECT|INSERT INTO|UPDATE [a-z]|DELETE FROM|CREATE TABLE)\b/.test(s), `SQL outside repository: ${f}`); }

const manifest=YAML.parse(fs.readFileSync('module.yaml','utf8'));
for(const [scenario,tests] of Object.entries(manifest.scenario_tests)){
 assert(tests.length,`No test for ${scenario}`);
 for(const reference of tests){const [file,name]=reference.split('#');assert(fs.readFileSync(file,'utf8').includes(`func ${name}(`),`Missing scenario test: ${reference}`);}
}
console.log('PASS: repository SQL boundary and registered scenario functions');

// The public build can use only this module and the public platform repository
// from the StuHelper organization. Backend owns all private provider bridges.
const modules = execFileSync('go', ['list', '-m', '-f', '{{.Path}}', 'all'], {encoding:'utf8', env:{...process.env,GOWORK:'off'}}).trim().split('\n');
const publicModules = new Set(['github.com/StuHelper/stuhelper-groups', 'github.com/StuHelper/stuhelper-platform']);
for (const name of modules) if (name.startsWith('github.com/StuHelper/')) assert(publicModules.has(name), `Non-public-build dependency: ${name}`);
assert.deepEqual(Object.keys(JSON.parse(fs.readFileSync('candidate-sources.json', 'utf8'))), ['platform']);
const workflow = YAML.parse(fs.readFileSync('.github/workflows/checks.yml', 'utf8'));
for (const job of Object.values(workflow.jobs)) for (const step of job.steps ?? []) {
 if (step.uses?.startsWith('actions/checkout@') && step.with?.repository) assert.equal(step.with.repository, 'StuHelper/stuhelper-platform');
}
console.log('PASS: public source dependency boundary');
