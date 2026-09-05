import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { register } from 'node:module';
import { test } from 'node:test';
register('./stub-loader.mjs', import.meta.url);

test('read-only child exposes response model evidence in every tool mode', async () => {
 const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pix-model-evidence-'));
 const saved = {...process.env};
 try {
  fs.mkdirSync(path.join(dir, 'agents'));
  fs.writeFileSync(path.join(dir, 'agents/review.md'), '---\ndescription: reviewer\ntools: read\nmodel: expected/model\n---\nReview.\n');
  const child = path.join(dir, 'child.mjs');
  fs.writeFileSync(child, `console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content:[{type:'text',text:'I am an imaginary model.'}],stopReason:'endTurn'}}));`);
  process.env.PI_TEST_AGENT_DIR = dir;
  process.env.PI_SUBAGENT_PI_COMMAND = `${process.execPath} ${child}`;
  delete process.env.PI_SUBAGENT_DISABLED;
  delete process.env.PI_SUBAGENT_DEPTH;
  const mod = await import('../extensions/subagents.ts?model-evidence');
  let tool;
  mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  for (const params of [{agent:'review',task:'Review.'},{tasks:[{agent:'review',task:'Review.'}]},{chain:[{agent:'review',task:'Review.'}]}]) {
   const result = await tool.execute('test',params,new AbortController().signal,()=>{},{cwd:dir});
   assert.ok(!result.isError, JSON.stringify(result));
   const text = result.content.map(c=>c.text || '').join('\n');
   assert.match(text,/model observed: actual\/response-model/);
   assert.doesNotMatch(text,/model observed: expected\/model/);
  }
 } finally {
  for(const key of Object.keys(process.env)) if(!(key in saved)) delete process.env[key];
  Object.assign(process.env,saved);
  fs.rmSync(dir,{recursive:true,force:true});
 }
});
