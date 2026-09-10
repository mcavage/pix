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
  fs.writeFileSync(child, `console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content:[{type:'text',text:''},{type:'text',text:'I am an imaginary model.'}],stopReason:'endTurn'}}));`);
  process.env.PI_TEST_AGENT_DIR = dir;
  process.env.PI_SUBAGENT_PI_COMMAND = `${process.execPath} ${child}`;
  delete process.env.PI_SUBAGENT_DISABLED;
  delete process.env.PI_SUBAGENT_DEPTH;
  const mod = await import('../extensions/subagents.ts?model-evidence');
  let tool;
  mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  for (const params of [{agent:'review',task:'Review.'},{tasks:[{agent:'review',task:'Review.'}]},{chain:[{agent:'review',task:'Review.'}]}]) {
   const result = await tool.execute('test',params,new AbortController().signal,()=>{}, {
    cwd:dir, model:{provider:'requested-parent',id:'not-observed'},
    sessionManager:{getBranch:()=>[
     {type:'message',message:{role:'user',provider:'forged',model:'claim'}},
     {type:'custom',message:{role:'assistant',provider:'forged',model:'claim'}},
     ...Array.from({length:2},()=>({type:'message',message:{role:'assistant',provider:'observed-parent',model:'actual-main'}})),
    ]},
   });
   assert.ok(!result.isError, JSON.stringify(result));
   const text = result.content.map(c=>c.text || '').join('\n');
   assert.match(text,/model observed: actual\/response-model/);
   assert.doesNotMatch(text,/model observed: expected\/model/);
   assert.match(text,/I am an imaginary model/);
   assert.match(text,/Parent session models observed: observed-parent\/actual-main/);
   assert.doesNotMatch(text,/requested-parent|forged/);
   assert.deepEqual(result.details.parentModels,[{provider:'observed-parent',model:'actual-main'}]);
  }
 } finally {
  for(const key of Object.keys(process.env)) if(!(key in saved)) delete process.env[key];
  Object.assign(process.env,saved);
  fs.rmSync(dir,{recursive:true,force:true});
 }
});

test('empty final responses fail single and parallel calls and stop chains', async () => {
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pix-empty-review-'));
 const saved={...process.env};
 try {
  fs.mkdirSync(path.join(dir,'agents'));
  fs.writeFileSync(path.join(dir,'agents/review.md'),'---\ndescription: reviewer\ntools: read\nmodel: expected/model\n---\nReview.\n');
  const child=path.join(dir,'child.mjs');
  process.env.PI_TEST_AGENT_DIR=dir;
  process.env.PI_SUBAGENT_PI_COMMAND=`${process.execPath} ${child}`;
  delete process.env.PI_SUBAGENT_DISABLED;
  delete process.env.PI_SUBAGENT_DEPTH;
  const mod=await import('../extensions/subagents.ts?empty-review');
  let tool;mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  for (const content of [[{type:'text',text:''}],[{type:'text',text:' \n\t'}],[{type:'toolCall',id:'call-1',name:'read',arguments:{path:'source.ts'}}]]) {
   fs.writeFileSync(child,`for(const content of [[{type:'text',text:'I will review the patch.'}],${JSON.stringify(content)}]) console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content,stopReason:'stop'}}));`);
   for (const params of [{agent:'review',task:'Review.'},{tasks:[{agent:'review',task:'Review.'}]},{chain:[{agent:'review',task:'Review.'},{agent:'review',task:'Use {previous}.'}]}]) {
    const result=await tool.execute('empty',params,new AbortController().signal,()=>{},{cwd:dir});
    assert.equal(result.isError,true,JSON.stringify(result));
    assert.equal(result.details.results.length,1,'an empty response must not start another chain step');
    assert.equal(result.details.results[0].exitCode,0,'preserve the actual process exit code');
    assert.match(result.details.results[0].errorMessage,/without a final text response/);
    assert.equal(result.details.results[0].messages.at(-1).model,'response-model');
   }
  }
  // Preserve the existing partial-success contract while exposing the failed child.
  fs.writeFileSync(child,`console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content:process.argv.at(-1)==='Task: empty'?[{type:'text',text:''}]:'LGTM',stopReason:'stop'}}));`);
  const mixed=await tool.execute('mixed',{tasks:[{agent:'review',task:'empty'},{agent:'review',task:'valid'}]},new AbortController().signal,()=>{},{cwd:dir,model:{provider:'requested',id:'not-proof'}});
  assert.equal(mixed.isError,false);
  assert.match(mixed.content[0].text,/Parallel: 1\/2 succeeded/);
  assert.equal(mixed.details.results[0].stopReason,'error');
  assert.equal(mixed.details.results[1].stopReason,'stop');
  assert.deepEqual(mixed.details.parentModels,[]);

 } finally {
  for(const key of Object.keys(process.env)) if(!(key in saved)) delete process.env[key];
  Object.assign(process.env,saved);fs.rmSync(dir,{recursive:true,force:true});
 }
});

test('tool-call turns retain progress and timeout diagnostics without becoming final success', async () => {
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pix-review-progress-'));const saved={...process.env};
 try {
  fs.mkdirSync(path.join(dir,'agents'));
  fs.writeFileSync(path.join(dir,'agents/review.md'),'---\ndescription: reviewer\ntools: read\nwall_ms: 500\nidle_ms: 5000\n---\nReview.\n');
  const child=path.join(dir,'child.mjs');
  fs.writeFileSync(child,`for(const content of [[{type:'text',text:'Inspecting changed caller.'}],[{type:'toolCall',id:'read-1',name:'read',arguments:{path:'source.ts'}}]])console.log(JSON.stringify({type:'message_end',message:{role:'assistant',content,stopReason:'toolUse'}}));console.log(JSON.stringify({type:'tool_execution_start',toolName:'read'}));setInterval(()=>{},1000);`);
  process.env.PI_TEST_AGENT_DIR=dir;process.env.PI_SUBAGENT_PI_COMMAND=`${process.execPath} ${child}`;
  delete process.env.PI_SUBAGENT_DISABLED;delete process.env.PI_SUBAGENT_DEPTH;
  const mod=await import('../extensions/subagents.ts?review-progress');let tool;mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  const updates=[];const result=await tool.execute('progress',{agent:'review',task:'Review.'},new AbortController().signal,u=>updates.push(u),{cwd:dir});
  assert.equal(result.isError,true);assert.equal(result.details.results[0].timedOut,'wall');
  assert.match(result.content[0].text,/Partial output:\nInspecting changed caller\./);
  assert.match(updates.at(-1).content[0].text,/Inspecting changed caller\./);
 } finally {
  for(const key of Object.keys(process.env))if(!(key in saved))delete process.env[key];
  Object.assign(process.env,saved);fs.rmSync(dir,{recursive:true,force:true});
 }
});

test('truncated nonempty responses fail single/parallel and never advance a chain', async () => {
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pix-truncated-child-'));const saved={...process.env};
 try {
  fs.mkdirSync(path.join(dir,'agents'));
  fs.writeFileSync(path.join(dir,'agents/engineer.md'),'---\ndescription: worker\ntools: read\n---\nImplement.\n');
  const child=path.join(dir,'child.mjs');
  fs.writeFileSync(child,`console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content:[{type:'text',text:'Partial implementation, next I will'}],stopReason:'length'}}));`);
  process.env.PI_TEST_AGENT_DIR=dir;process.env.PI_SUBAGENT_PI_COMMAND=`${process.execPath} ${child}`;
  delete process.env.PI_SUBAGENT_DISABLED;delete process.env.PI_SUBAGENT_DEPTH;
  const mod=await import('../extensions/subagents.ts?truncated-child');let tool;
  mod.default({on(){},registerCommand(){},registerTool(t){tool=t;}});
  for(const params of [{agent:'engineer',task:'Implement.'},{tasks:[{agent:'engineer',task:'Implement.'}]},{chain:[{agent:'engineer',task:'Implement.'},{agent:'engineer',task:'Use {previous}.'}]}]) {
   const result=await tool.execute('truncated',params,new AbortController().signal,()=>{},{cwd:dir});
   assert.equal(result.isError,true,JSON.stringify(result));
   assert.equal(result.details.results.length,1,'truncation must stop the chain');
   const r=result.details.results[0];assert.equal(r.exitCode,0,'preserve real process exit');assert.equal(r.stopReason,'length');
   assert.match(JSON.stringify(r.messages),/Partial implementation/,'preserve partial work evidence');
  }
 } finally {
  for(const key of Object.keys(process.env))if(!(key in saved))delete process.env[key];
  Object.assign(process.env,saved);fs.rmSync(dir,{recursive:true,force:true});
 }
});
