const test = require('node:test');
const assert = require('node:assert/strict');
const { resolveServices, projectNameError, buildInfraPayload, previewInfrastructure, generateInfrastructure } = require('../lab-ui/app.js');
const { proposeInfrastructureWithAI, INFRA_SERVICES } = require('../lab-ui/app.js');
const { architectureFromPreview, designClarification, proposalPresentation, architectureModel, configurationNotices } = require('../lab-ui/app.js');
const { prepareAICandidate, commitAICandidate, aiProcessState, architectureFocus } = require('../lab-ui/app.js');
const {orderedCatalogueKeys,catalogueMatches,catalogueCounts,partitionSelection}=require('../lab-ui/app.js');
const {normalizeRuntimeModel,huggingFaceFiles,SERVICE_CATEGORIES}=require('../lab-ui/app.js');
const {privateAIProfile,restoreInfrastructureOptions,optionsFromAIProposal}=require('../lab-ui/app.js');
const preview = {status:'ok',is_new:true,retention:'none',new_yaml:'services:\n  nginx:\n    image: nginx:alpine\n'};
const exportResult = project => ({status:'ok',project,deploy:false,retention:'none',compose:preview.new_yaml,archive_base64:'UEsDBAoAAAA='});

test('Minecraft chat has a real connector and keeps the chosen vLLM model visible',()=>{
 const payload=buildInfraPayload(['minecraft-ai','vllm'],null,{vllmModel:'Qwen/Qwen2.5-3B-Instruct'});
 assert.ok(payload.services.some(service=>service.key==='minecraft'));
 assert.ok(!payload.services.some(service=>service.key==='ollama'));
 const presentation=proposalPresentation(payload);
 assert.match(presentation.title,/assistant IA dans Minecraft/);
 const vllm=presentation.groups.flatMap(group=>group.services).find(service=>service.key==='vllm');
 assert.match(vllm.detail,/Qwen\/Qwen2.5-3B-Instruct/);
});

test('architecture uses the final backend networks and model rather than inferring from YAML headings',()=>{
 const graph={version:1,services:[{key:'vllm',image:'vllm/vllm-openai:latest',networks:['autodocker-ai'],depends_on:[],ports:[]},{key:'minecraft-ai',networks:['autodocker-ai'],depends_on:['vllm'],ports:[]}]};
 const parsed=architectureFromPreview('unrecognized YAML layout',{},graph);
 const model=architectureModel({...parsed,ai:{engine:'vllm',model:'Qwen/Qwen2.5-3B-Instruct',connections:[{from:'minecraft-ai',to:'vllm',kind:'inference',endpoint:'http://vllm:8000/v1'}]}});
 const focus=architectureFocus(model,'vllm');
 assert.deepEqual(focus.node.networks,['autodocker-ai']);assert.equal(focus.node.runtimeModel,'Qwen/Qwen2.5-3B-Instruct');assert.equal(focus.node.internalEndpoint,'http://vllm:8000/v1');
});

test('an old server cannot silently drop the Minecraft chat connector',()=>{
 const {verifyConfiguredPreview}=require('../lab-ui/app.js');
 const payload=buildInfraPayload(['minecraft-ai','vllm']);
 assert.throws(()=>verifyConfiguredPreview(preview,payload),/chat Minecraft/);
 assert.doesNotThrow(()=>verifyConfiguredPreview({...preview,ingress:{ai:{version:2,minecraft_chat:{version:1}}}},payload));
});

test('a catalogue profile whose image disappeared cannot generate a broken deployment',()=>{
 assert.ok(INFRA_SERVICES.moodle.unavailable);assert.throws(()=>buildInfraPayload(['moodle']),/indisponible/);
});

test('AI design checks and manual steps survive proposal acceptance independently of model prose',async()=>{
 const response={confidence:'validated',payload:{services:[{key:'nginx'}]},design_checks:[{title:'Structure contrôlée',detail:'Réseaux et dépendances vérifiés.'}],manual_steps:['Tester sur le serveur cible.']};
 const proposal=await proposeInfrastructureWithAI(async()=>response,'Un service Nginx');
 const candidate=prepareAICandidate(proposal,'Un service Nginx');
 const committed=commitAICandidate(candidate,'Un service Nginx');
 assert.deepEqual(committed.aiMetadata.design_checks,response.design_checks);assert.deepEqual(committed.aiMetadata.manual_steps,response.manual_steps);
 response.design_checks[0].title='mutated';assert.equal(committed.aiMetadata.design_checks[0].title,'Structure contrôlée');
});

test('client identity and supervision remain opt-in and preserve infrastructure topology',()=>{
 const options=restoreInfrastructureOptions({clientEnabled:true,clientName:'Atelier client',clientAccent:'#4f8cff',supervisionEnabled:true,supervisionGPU:false,supervisionDisk:'/srv',supervisionBackup:''});
 const payload=buildInfraPayload(['wordpress'],null,options),base=buildInfraPayload(['wordpress']);
 assert.deepEqual(payload.services,base.services);assert.equal(base.client,undefined);assert.equal(base.supervision,undefined);
 assert.equal(payload.client.name,'Atelier client');assert.equal(payload.supervision.gpu,false);assert.equal(payload.supervision.backup_status_file,'');assert.equal(payload.supervision.backup_max_hours,30);
});
test('incomplete delivery settings fail before a preview API request',async()=>{
 let calls=0;const api=async()=>{calls++;return preview;};
 await assert.rejects(previewInfrastructure(api,'client-check',['nginx'],null,{clientEnabled:true,clientName:''}),/nom court/);
 await assert.rejects(previewInfrastructure(api,'client-check',['nginx'],null,{supervisionEnabled:true,supervisionDisk:'/srv',supervisionDiskWarning:200}),/seuils/);
 assert.equal(calls,0);
});
test('an old backend cannot silently drop delivery files',()=>{
 const {verifyConfiguredPreview}=require('../lab-ui/app.js');
 assert.throws(()=>verifyConfiguredPreview(preview,{client:{name:'Client'}}),/serveur/);
 assert.throws(()=>verifyConfiguredPreview(preview,{supervision:{}}),/serveur/);
 assert.doesNotThrow(()=>verifyConfiguredPreview({...preview,ingress:{client_delivery:{version:1}}},{client:{name:'Client'}}));
});
test('delivery rejects remote and active-content logos and invalid accent colors',()=>{
 const {clientDeliveryPayload}=require('../lab-ui/app.js');
 for(const logo of ['https://example.test/logo.png','data:image/svg+xml;base64,PHN2Zz4='])assert.throws(()=>clientDeliveryPayload({clientEnabled:true,clientName:'Client',clientLogo:logo}),/logo/);
 assert.throws(()=>clientDeliveryPayload({clientEnabled:true,clientName:'Client',clientAccent:'red;display:none'}),/Couleur/);
});
test('delivery preferences survive in-memory configuration and adding a private AI base',()=>{
 const restored=restoreInfrastructureOptions({clientEnabled:true,clientName:'Client conservé',supervisionEnabled:true,supervisionBackupHours:'36'});
 const profile=privateAIProfile(['nginx'],restored);
 assert.equal(profile.options.clientName,'Client conservé');assert.equal(profile.options.supervisionBackupHours,36);
 const payload=buildInfraPayload(profile.keys,null,profile.options);assert.equal(payload.client.name,'Client conservé');assert.equal(payload.supervision.backup_max_hours,36);
});

test('the private AI profile adds a connected vision platform without dropping unrelated choices',()=>{
 const before=['nginx','haproxy'],options={model:'my-ollama:tag',domain:'infra.local',port:8088};
 const profile=privateAIProfile(before,options),payload=buildInfraPayload(profile.keys,null,profile.options);
 assert.ok(profile.keys.includes('nginx'));assert.ok(!profile.keys.includes('haproxy'));
 for(const key of ['vllm','open-webui','qdrant','tei','caddy'])assert.ok(profile.keys.includes(key));
 assert.equal(payload.ai_stack.vision,true);assert.equal(payload.ai_stack.private_platform,true);assert.equal(payload.ai_stack.max_context,8192);
 assert.equal(payload.ai_stack.model,'Qwen/Qwen2.5-VL-3B-Instruct');assert.equal(payload.ingress.tls,'internal');assert.equal(payload.ingress.port,8443);
 assert.deepEqual(before,['nginx','haproxy']);assert.equal(options.port,8088);
});
test('advanced AI settings restore correctly including older numeric strings',()=>{
 const options=restoreInfrastructureOptions({port:'18443',maxContext:8192,vision:true,privatePlatform:true,gpuMemory:0.7,concurrency:4,reasoningParser:'qwen3'});
 assert.equal(options.port,18443);assert.equal(options.vision,true);assert.equal(options.gpuMemory,0.7);
 assert.throws(()=>buildInfraPayload(['vllm'],null,{...options,concurrency:100}),/simultanées/);
});
test('accepting an AI architecture can transfer explicit vision, capacity and HTTPS options',()=>{
 const previous={vision:false,privatePlatform:false,model:'existing:tag'};
 const proposal={ai_stack:{engine:'vllm',model:'Qwen/Qwen2.5-VL-3B-Instruct',vision:true,private_platform:true,max_context:8192,concurrency:3},ingress:{proxy:'caddy',domain:'local.test',port:18443,tls:'internal'}};
 const next=optionsFromAIProposal(proposal,previous);
 assert.equal(next.vision,true);assert.equal(next.privatePlatform,true);assert.equal(next.port,18443);assert.equal(next.tls,'internal');
 assert.equal(next.model,'existing:tag');assert.equal(previous.vision,false);
});
test('an old server cannot ignore private HTTPS or the new vision settings',()=>{
 const {verifyConfiguredPreview}=require('../lab-ui/app.js');const profile=privateAIProfile([],{}),payload=buildInfraPayload(profile.keys,null,profile.options);
 assert.throws(()=>verifyConfiguredPreview({new_yaml:'AUTODOCKER_PROXY_CONFIG:',ingress:{ai:{version:1},ingress:{protocol:'http'}}},payload),/profil IA|HTTPS/);
 assert.doesNotThrow(()=>verifyConfiguredPreview({new_yaml:'AUTODOCKER_PROXY_CONFIG:',ingress:{ai:{version:2},ingress:{protocol:'https'}}},payload));
});

test('professional AI consumers use the selected engine and complete RAG dependencies',()=>{
 const keys=resolveServices(['vllm','open-webui','anythingllm','hermes-agent','openclaw']);
 assert.ok(!keys.includes('ollama'));for(const key of ['vllm','tei','qdrant'])assert.ok(keys.includes(key));
 assert.ok(resolveServices(['hermes-agent']).includes('ollama'));
 const p=buildInfraPayload(['vllm','open-webui'],null,{vllmModel:'Qwen/Qwen2.5-3B-Instruct'});
 assert.equal(p.ai_stack.engine,'vllm');assert.equal(p.ai_stack.model,'Qwen/Qwen2.5-3B-Instruct');
 assert.equal(p.services.find(s=>s.key==='vllm').image,'vllm/vllm-openai:latest');
});
test('AI includes agents and RAG while specialised filters remain precise',()=>{
 const all=catalogueMatches(INFRA_SERVICES,[],'ai');
 for(const key of ['vllm','ollama','open-webui','hermes-agent','openclaw','anythingllm','tei','qdrant'])assert.ok(all.includes(key));
 assert.deepEqual(new Set(catalogueMatches(INFRA_SERVICES,[],'agents')),new Set(['hermes-agent','openclaw']));
 const count=catalogueCounts(INFRA_SERVICES,[]);assert.equal(count.ai,all.length);assert.equal(count.agents,2);
});
test('vLLM rejects GGUF and invalid repositories without changing Ollama preferences',()=>{
 for(const vllmModel of ['../file','author/model-GGUF','author/model/file','author/model;bad'])assert.throws(()=>buildInfraPayload(['vllm'],null,{vllmModel}),/Hugging Face/);
 const p=buildInfraPayload(['vllm','open-webui'],null,{vllmModel:'https://huggingface.co/Qwen/Qwen2.5-3B-Instruct',model:'hf.co/author/old:Q4_K_M'});
 assert.equal(p.ai_stack.model,'Qwen/Qwen2.5-3B-Instruct');
});
test('old servers cannot silently discard professional AI connections',async()=>{
 let writes=0;const payload=buildInfraPayload(['anythingllm']);
 await assert.rejects(generateInfrastructure(async path=>{if(path==='/api/generate')writes++;return preview;},{project:'ai-pro',payload},false,false),/connexions IA professionnelles/);
 assert.equal(writes,0);
});
test('the review displays AI integration edges only from actual server metadata',()=>{
 const base={declaredDependenciesOnly:true,services:[{key:'open-webui'},{key:'vllm'}]};
 assert.equal(architectureModel(base).edges.length,0);
 const actual=architectureModel({...base,ai:{connections:[{from:'open-webui',to:'vllm',kind:'inference',endpoint:'http://vllm:8000/v1'}]}});
 assert.equal(actual.edges[0].type,'inference');assert.equal(actual.edges[0].endpoint,'http://vllm:8000/v1');
});

test('WordPress, Nextcloud and local AI include their required backends once', () => {
  assert.deepEqual(resolveServices(['wordpress','nextcloud','open-webui','mariadb']), ['wordpress','mariadb','nextcloud','open-webui','ollama']);
  assert.deepEqual(resolveServices(['gitea']), ['gitea','postgres']);
  assert.deepEqual(resolveServices(['nginx','unknown','constructor','nginx']), ['nginx']);
});
test('unsafe or empty project names fail before reaching the server', async () => {
  let calls = 0;
  for (const name of ['', '../site', 'Mon Site', '<script>', 'x'.repeat(64)]) {
    assert.ok(projectNameError(name));
    await assert.rejects(previewInfrastructure(async () => { calls++; }, name, ['nginx']));
  }
  assert.equal(calls,0);
  assert.equal(projectNameError('mon-site_2'),'');
});
test('an empty selection never triggers a preview request', async () => {
  let calls = 0;
  await assert.rejects(previewInfrastructure(async () => { calls++; },'mon-site',[]),/Choisissez/);
  assert.equal(calls,0);
});
test('names remain reusable while legacy persistent previews are rejected', async () => {
  const result=await previewInfrastructure(async()=>({...preview,is_new:false}),'mon-site',['nginx']);
  assert.equal(result.project,'mon-site');
  await assert.rejects(previewInfrastructure(async()=>({...preview,retention:undefined}),'mon-site',['nginx']),/sans enregistrer/);
});
test('an offline machine can generate files without starting containers', async () => {
  const calls = [];
  const configuration = {project:'mon-site',payload:buildInfraPayload(['nginx'])};
  const api = async(path,body) => {calls.push({path,body}); return path.endsWith('/preview')?preview:exportResult('mon-site');};
  const result = await generateInfrastructure(api,configuration,false,false);
  assert.equal(result.deploy,false);
  assert.deepEqual(calls.map(x=>x.path),['/api/generate/preview','/api/generate']);
  assert.equal(calls[1].body.deploy,undefined);
  assert.equal(calls[1].body.payload.services[0].network,'app');
});
test('failed, interrupted, malformed or stale previews never lead to a write', async () => {
  for (const response of [{...preview,retention:undefined},{status:'error',error:'Erreur de validation'},{status:'ok',is_new:true}]) {
    let writes=0;
    await assert.rejects(generateInfrastructure(async path=>{if(path==='/api/generate') writes++; return response;},{project:'site',payload:{}},false,false));
    assert.equal(writes,0);
  }
  let calls=0;
  await assert.rejects(generateInfrastructure(async()=>{calls++;throw new Error('Réseau interrompu');},{project:'site',payload:{}},false,false));
  assert.equal(calls,1);
});
test('file generation never starts containers regardless of Docker availability', async () => {
  let calls=0;
  await assert.rejects(generateInfrastructure(async()=>{calls++;},{project:'site',payload:{}},true,true),/AutoDocker génère les fichiers/);
  assert.equal(calls,0);
});
test('a server generation failure or incomplete archive stays a failure', async () => {
  await assert.rejects(generateInfrastructure(async path=>path.endsWith('/preview')?preview:{status:'error',error:'Génération indisponible'},{project:'site',payload:{}}),/Génération indisponible/);
  await assert.rejects(generateInfrastructure(async path=>path.endsWith('/preview')?preview:{...exportResult('site'),archive_base64:undefined},{project:'site',payload:{}}),/archive reçue est incomplète/);
});

test('manual adjustments retain AI environment, ports and persistent volumes', () => {
  const ai={services:[{key:'redis',network:'backend',env:{MODE:'test'},ports:['6380:6379'],volumes:['cache:/data']}],addressing:{networks:[{name:'backend',cidr:'172.28.0.0/24',gateway:'172.28.0.1'}]}};
  const payload=buildInfraPayload(['redis','nginx'],ai);
  assert.equal(payload.services.find(s=>s.key==='redis').env.MODE,'test');
  assert.deepEqual(payload.services.find(s=>s.key==='redis').ports,['6380:6379']);
  assert.equal(payload.services.find(s=>s.key==='nginx').network,'backend');
  assert.deepEqual(payload.services.find(s=>s.key==='redis').volumes,['cache:/data']);
  assert.deepEqual(ai.services.map(s=>s.key),['redis']);
});

test('new catalogue entries contain the Docker specification the generator needs', () => {
  const payload=buildInfraPayload(['caddy','rabbitmq','memcached','meilisearch']);
  assert.equal(payload.services.length,4);
  for(const service of payload.services){assert.ok(service.image);assert.ok(service.ports.length);}
  const rabbit=payload.services.find(s=>s.key==='rabbitmq');
  assert.equal(rabbit.env.RABBITMQ_DEFAULT_PASS,'__AUTO_PASSWORD__');
  assert.deepEqual(rabbit.volumes,['rabbitmq_data:/var/lib/rabbitmq']);
  assert.deepEqual(resolveServices(['wordpress','grafana']),['wordpress','mariadb','grafana','prometheus']);
});

test('AI clarification is a question, not an empty successful architecture', async () => {
  const result = await proposeInfrastructureWithAI(async () => ({questions:['Quel jeu voulez-vous héberger ?'],source:'gemini'}),'Une infra de jeu');
  assert.deepEqual(result.questions,['Quel jeu voulez-vous héberger ?']);
  assert.equal(result.payload,undefined);
});

test('AI errors and explanations remain provider independent', async () => {
  await assert.rejects(proposeInfrastructureWithAI(async()=>{throw new Error('Gemini indisponible');},'WordPress'),/Gemini indisponible/);
  const payload={services:[{key:'nginx'}]};
  const result=await proposeInfrastructureWithAI(async()=>({payload,source:'openrouter',model:'cohere/north-mini-code:free',confidence:'validated',summary:'Un site statique',reasons:{nginx:'Servir vos pages'}}),'Un site');
  assert.equal(result.summary,'Un site statique');assert.equal(result.reasons.nginx,'Servir vos pages');assert.equal(result.validated,true);
  assert.equal(result.source,'openrouter');assert.equal(result.model,'cohere/north-mini-code:free');
  await assert.rejects(proposeInfrastructureWithAI(async()=>({payload,source:'lmstudio',confidence:'high'}),'Un site'),/ancien moteur/);
});

test('AI Grafana with Loki does not acquire an unwanted Prometheus dependency', () => {
  INFRA_SERVICES.loki={name:'Loki',description:'Logs',category:'infra'};
  try {
    const proposal={services:[{key:'grafana',network:'app',depends_on:['loki']},{key:'loki',network:'app',image:'grafana/loki:3.0.0'}]};
    const payload=buildInfraPayload(['grafana','loki'],proposal);
    assert.deepEqual(payload.services.map(s=>s.key),['grafana','loki']);
    assert.equal(payload.services[1].image,'grafana/loki:3.0.0');
  } finally { delete INFRA_SERVICES.loki; }
});

test('an ambiguous gaming request asks before any model is called', async () => {
  let calls=0;
  const result=await proposeInfrastructureWithAI(async()=>{calls++;throw new Error('Must not call the model yet');},"une infra pour du jeu vidéo que j'aimerais partager");
  assert.equal(calls,0);
  assert.ok(result.questions[0].includes('Quel jeu'));
  assert.deepEqual(result.choices.map(choice=>choice.label),['Minecraft','Valheim','Autre jeu','Jeu dans le navigateur']);
  for (const text of ['Un serveur Minecraft pour mes amis','Un jeu vidéo dans le navigateur','Un serveur de jeu Valheim', 'Un serveur pour le jeu Rocket League']) assert.equal(designClarification(text),null);
});

test('specific game clarification reaches the selected provider instead of a preset', async () => {
  const prompt="une infra pour du jeu vidéo que j'aimerais partager\nPrécision : Je souhaite héberger un serveur Minecraft.";
  let received='';
  const result=await proposeInfrastructureWithAI(async(path,body)=>{received=body.prompt;return {payload:{services:[{key:'minecraft',image:'example/game:1'}]},source:'lmstudio',confidence:'validated'};},prompt);
  assert.equal(received,prompt);assert.equal(result.payload.services[0].key,'minecraft');
});

test('shared networks never create invented application connections', () => {
  const payload=buildInfraPayload(['wordpress','redis','uptime-kuma']);
  const model=architectureModel(payload);
  assert.deepEqual(model.edges,[{from:'wordpress',to:'mariadb'}]);
  assert.equal(model.nodes.length,4);assert.equal(model.networks[0].name,'app');
  const notices=configurationNotices(payload);
  assert.ok(notices.some(notice=>notice.type==='dependency'&&notice.key==='mariadb'));
  assert.ok(notices.some(notice=>notice.type==='configuration'&&notice.text.includes('Uptime Kuma')));
});

test('the architecture represents declared custom dependencies without forcing metrics', () => {
  const payload={services:[{key:'grafana',network:'observability',depends_on:['loki']},{key:'loki',network:'observability'}],addressing:{networks:[{name:'observability'}]}};
  assert.deepEqual(architectureModel(payload).edges,[{from:'grafana',to:'loki'}]);
});

test('a proposal describes only selected services and does not repeat unverified AI prose', () => {
  const payload=buildInfraPayload(['wordpress','grafana']);
  const result=proposalPresentation(payload,{summary:'Nginx protège le site.',reasons:{wordpress:'Nginx protège le site.'}});
  assert.equal(result.title,'Votre site WordPress et ses services');
  assert.match(result.summary,/WordPress.*MariaDB.*Grafana.*Prometheus/);
  assert.doesNotMatch(result.summary,/Nginx/);
  assert.deepEqual(result.groups.map(group=>group.name),['Applications','Bases de données et cache','Suivi et observabilité']);
  assert.doesNotMatch(result.groups[0].services[0].detail,/Nginx/);
});

test('review diagrams use emitted Compose services including auxiliary containers', () => {
  const yaml='networks:\n  app:\n    driver: bridge\nservices:\n  wordpress:\n    depends_on:\n    - mariadb\n    networks:\n      app:\n        ipv4_address: 172.25.0.100\n  mariadb:\n    image: mariadb:11\n  adminer:\n    image: adminer:latest\n  grafana:\n    image: grafana/grafana:latest\n  prometheus:\n    image: prom/prometheus:latest\nvolumes:\n  data: {}\n';
  const model=architectureModel(architectureFromPreview(yaml,buildInfraPayload(['wordpress','grafana'])));
  assert.deepEqual(model.nodes.map(node=>node.key),['wordpress','mariadb','adminer','grafana','prometheus']);
  assert.deepEqual(model.edges,[{from:'wordpress',to:'mariadb'}]);
  assert.equal(model.nodes[0].network,'app');
  assert.equal(buildInfraPayload(['wordpress'],{ui:{db:true}}).ui.db,false);
  assert.equal(buildInfraPayload(['wordpress','adminer']).ui.db,true);
  assert.deepEqual(buildInfraPayload(['adminer']).services[0].ports,['8085:8080']);
});

test('review understands conditional dependencies and refuses an unreadable diagram', () => {
  const yaml='services:\n  frontend:\n    depends_on:\n      backend:\n        condition: service_healthy\n    networks:\n    - app\n  backend:\n    image: example/backend:1\n';
  const model=architectureModel(architectureFromPreview(yaml,{}));
  assert.deepEqual(model.edges,[{from:'frontend',to:'backend'}]);
  assert.throws(()=>architectureFromPreview('invalid',{}),/schéma/);
  const quoted=architectureModel(architectureFromPreview('services:\n  "true":\n    depends_on:\n    - "123"\n  "123":\n    image: example/backend:1\n',{}));
  assert.deepEqual(quoted.edges,[{from:'true',to:'123'}]);
  assert.throws(()=>architectureFromPreview('services:\n  valid:\n    image: example/service:1\n  "unsupported key":\n    image: example/service:1\n',{}),/format du schéma/);
});

test('an AI candidate remains separate from the committed selection until explicitly used', () => {
  const committed={keys:['gitea'],proposal:buildInfraPayload(['gitea'])};
  const original=structuredClone(committed);
  const data={validated:true,payload:buildInfraPayload(['wordpress']),summary:'Un site WordPress',reasons:{wordpress:'Publier des articles'}};
  const candidate=prepareAICandidate(data,'Un site WordPress');
  assert.deepEqual(committed,original);
  data.payload.services[0].key='redis';
  assert.equal(candidate.payload.services[0].key,'wordpress');
  const next=commitAICandidate(candidate,'Un site WordPress');
  assert.deepEqual(next.keys,['wordpress','mariadb']);
  assert.deepEqual(committed,original);
  next.proposal.services[0].key='nginx';
  assert.equal(candidate.payload.services[0].key,'wordpress');
});

test('an edited request cannot commit a stale AI proposal', () => {
  const candidate=prepareAICandidate({validated:true,payload:buildInfraPayload(['wordpress'])},'Un blog');
  assert.throws(()=>commitAICandidate(candidate,'Un serveur de jeu'),/demande a changé/);
  assert.throws(()=>prepareAICandidate({validated:false,payload:buildInfraPayload(['nginx'])},'Un site'),/vérifiée/);
  assert.equal(aiProcessState({prompt:'Un blog',candidate}).canUse,true);
  assert.equal(aiProcessState({prompt:'Un jeu',candidate}).canUse,false);
  candidate.applied=true;
  assert.equal(aiProcessState({prompt:'Un blog',candidate}).title,'Architecture retenue');
});

test('AI progress describes its own process and never incorporates the catalogue implicitly', () => {
  assert.equal(aiProcessState({prompt:'Un jeu'}).stage,0);
  assert.equal(aiProcessState({prompt:'Un jeu',questions:true}).stage,1);
  const pending=aiProcessState({prompt:'Un jeu',pending:true});
  assert.equal(pending.canUse,false);assert.equal(pending.stage,2);
  assert.match(pending.message,/catalogue reste inchangé/);
});

test('architecture exploration uses real dependencies rather than all network peers', () => {
  const yaml='services:\n  wordpress:\n    image: wordpress:latest\n    depends_on:\n    - mariadb\n    ports:\n    - "8082:80"\n    networks:\n      app:\n      backend:\n  mariadb:\n    image: mariadb:11\n    networks:\n      backend:\n  redis:\n    image: redis:alpine\n    networks:\n      app:\n  fail2ban:\n    image: example/fail2ban:1\n    network_mode: host\n';
  const model=architectureModel(architectureFromPreview(yaml,{}));
  const focus=architectureFocus(model,'wordpress');
  assert.deepEqual(focus.dependencies,['mariadb']);assert.deepEqual(focus.dependents,[]);
  assert.deepEqual(focus.neighbours,['wordpress','mariadb']);
  assert.deepEqual(focus.node.networks,['app','backend']);assert.deepEqual(focus.node.ports,['8082:80']);
  assert.equal(focus.node.image,'wordpress:latest');
  assert.deepEqual(architectureFocus(model,'mariadb').dependents,['wordpress']);
  assert.deepEqual(architectureFocus(model,'fail2ban').node.networks,[]);
  assert.equal(architectureFocus(model,'fail2ban').node.networkMode,'host');
  assert.equal(architectureFocus(model,'missing'),null);
});

test('the catalogue prioritises the requested common services without dropping others',()=>{
  const order=orderedCatalogueKeys(INFRA_SERVICES);
  assert.deepEqual(order.slice(0,4),['nginx','postgres','nextcloud','portainer']);
  assert.equal(new Set(order).size,Object.keys(INFRA_SERVICES).length);
});
test('selection and category filters remain active while searching',()=>{
  const selected=resolveServices(['wordpress','postgres']);
  assert.deepEqual(new Set(catalogueMatches(INFRA_SERVICES,selected,'selected','SQL')),new Set(['postgres','mariadb']));
  assert.deepEqual(catalogueMatches(INFRA_SERVICES,selected,'data','WordPress'),[]);
  const counts=catalogueCounts(INFRA_SERVICES,selected);
  assert.equal(counts.selected,3);assert.equal(counts.all,Object.keys(INFRA_SERVICES).length);
  assert.equal(catalogueCounts(INFRA_SERVICES,selected,'postgres').selected,1);
});
test('an explicitly selected database remains chosen rather than being relabelled automatic',()=>{
  assert.deepEqual(partitionSelection(['wordpress']),{chosen:['wordpress'],automatic:['mariadb']});
  assert.deepEqual(partitionSelection(['mariadb','wordpress']),{chosen:['mariadb','wordpress'],automatic:[]});
});
test('service details read actual named and read-only volumes without exposing environment values',()=>{
  const yaml='services:\n  nginx:\n    image: nginx:alpine\n    environment:\n      PASSWORD: should-not-appear\n    ports:\n    - "8080:80"\n    volumes:\n    - "site_data:/usr/share/nginx/html"\n    - ./nginx.conf:/etc/nginx/nginx.conf:ro\n';
  const service=architectureFromPreview(yaml,{}).services[0];
  assert.deepEqual(service.volumes,['site_data:/usr/share/nginx/html','./nginx.conf:/etc/nginx/nginx.conf:ro']);
  assert.deepEqual(service.ports,['8080:80']);
  assert.ok(!JSON.stringify(service).includes('should-not-appear'));
});

test('professional categories include proxies and keep all counts finite',()=>{
  const counts=catalogueCounts(INFRA_SERVICES,[]);
  assert.ok(counts.proxy>=4);assert.ok(counts.monitoring>=4);assert.ok(counts.dev>=3);
  assert.ok(Object.values(counts).every(Number.isFinite));
  for(const s of Object.values(INFRA_SERVICES)) assert.ok(Object.hasOwn(SERVICE_CATEGORIES,s.category));
});

test('a chosen model overrides both runtime entries without changing the AI proposal',()=>{
  const proposal={services:[{key:'open-webui',extra:{model:'old'},network:'front'},{key:'ollama',extra:{model:'old'},network:'back'}]};
  const model='hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M';
  const payload=buildInfraPayload(['open-webui'],proposal,{model});
  assert.equal(payload.services.find(s=>s.key==='open-webui').extra.model,model);
  assert.equal(payload.services.find(s=>s.key==='ollama').extra.model,model);
  assert.equal(proposal.services[0].extra.model,'old');
  assert.equal(normalizeRuntimeModel('https://huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M'),model);
  for(const bad of ['--help','qwen;whoami','hf.co/user/repo/tree/main','../repo'])assert.throws(()=>normalizeRuntimeModel(bad));
});

test('Hugging Face files exclude split models and vision projectors and prefer Q4_K_M',()=>{
  assert.deepEqual(huggingFaceFiles({siblings:[{rfilename:'model-Q8_0.gguf'},{rfilename:'model-Q4_K_M.gguf'},{rfilename:'model-00001-of-00002.gguf'},{rfilename:'mmproj-f16.gguf'},{rfilename:'README.md'}]}),['model-Q4_K_M.gguf','model-Q8_0.gguf']);
});

test('gateway settings travel with the payload and reject multiple proxies',()=>{
  const p=buildInfraPayload(['caddy','wordpress'],null,{domain:'infra.localhost',port:8099});
  assert.deepEqual(p.ingress,{proxy:'caddy',domain:'infra.localhost',port:8099});
  assert.ok(p.services.some(s=>s.key==='mariadb'));
  assert.throws(()=>buildInfraPayload(['caddy','traefik']),/seul reverse proxy/);
  assert.throws(()=>buildInfraPayload(['caddy'],null,{domain:'https://example.com'}));
  assert.throws(()=>buildInfraPayload(['caddy'],null,{port:80}));
});

test('an old server cannot silently ignore routing and create an unprotected infrastructure',async()=>{
  let writes=0;
  const api=async(route)=>{if(route==='/api/generate'){writes++;return {status:'ok'};}return {status:'ok',is_new:true,retention:'none',new_yaml:'services:\n  caddy:\n    image: caddy:2-alpine\n'};};
  await assert.rejects(previewInfrastructure(api,'test-pro',['caddy','wordpress']),/ancien générateur/);
  await assert.rejects(generateInfrastructure(api,{project:'test-pro',payload:buildInfraPayload(['caddy','wordpress'])},false,false),/ancien générateur/);
  assert.equal(writes,0);
});

test('known pro service specifications survive an AI proposal that contains just their keys',()=>{
 const payload=buildInfraPayload(['caddy','whoami'],{services:[{key:'caddy',network:'front'},{key:'whoami',network:'front'}]});
 assert.equal(payload.services.find(s=>s.key==='caddy').image,'caddy:2-alpine');
 assert.equal(payload.services.find(s=>s.key==='whoami').image,'traefik/whoami:v1.11');
 assert.doesNotThrow(()=>buildInfraPayload(['nginx'],null,{model:'invalid;stale'}));
});

test('architecture routing edges come from the server metadata and leave SQL dependencies intact',()=>{
 const graph=architectureModel({services:[{key:'caddy'},{key:'wordpress',depends_on:['mariadb']},{key:'mariadb'}],declaredDependenciesOnly:true,routing:{proxy:'caddy',routes:[{service:'wordpress',host:'wordpress.localhost',port:80}]}});
 assert.ok(graph.edges.some(e=>e.from==='caddy'&&e.to==='wordpress'&&e.type==='route'));
 assert.ok(graph.edges.some(e=>e.from==='wordpress'&&e.to==='mariadb'&&!e.type));
 assert.ok(!graph.edges.some(e=>e.from==='caddy'&&e.to==='mariadb'));
});

const {runtimePickerContext,runtimeModelPresentation,modelFileLabel}=require('../lab-ui/app.js');
test('model controls appear only for an active infrastructure containing Ollama',()=>{
 assert.equal(runtimePickerContext([]).visible,false);
 assert.equal(runtimePickerContext(['nginx','caddy']).visible,false);
 assert.deepEqual(runtimePickerContext(['ollama']),{visible:true,webUI:false});
 assert.deepEqual(runtimePickerContext(['open-webui']),{visible:true,webUI:true});
 assert.equal(runtimePickerContext(['open-webui'],null,'ai',{applied:false}).visible,false);
 assert.equal(runtimePickerContext(['open-webui'],null,'ai',{applied:true}).visible,true);
});
test('model selection distinguishes a readable name from its technical identifier',()=>{
 assert.equal(runtimeModelPresentation('qwen3:4b').title,'Qwen 3');
 const hf=runtimeModelPresentation('hf.co/Qwen/Qwen3-4B-GGUF:Qwen3-4B-Q4_K_M.gguf');
 assert.equal(hf.title,'Qwen3-4B');assert.equal(hf.detail,'Hugging Face · Q4_K_M');
 assert.equal(runtimeModelPresentation('').title,'Modèle par défaut');
 assert.match(modelFileLabel('Qwen3-4B-Q4_K_M.gguf'),/^Équilibrée · Q4_K_M/);
});


test('AI queue waits for its own completed result and reports progress',async()=>{
  const {waitForInfrastructureJob}=require('../lab-ui/app.js');
  const id='a'.repeat(48),states=[],calls=[];
  const responses=[{job:{id,state:'queued',position:2,active:3}},{job:{id,state:'running',position:0,active:3}},{job:{id,state:'succeeded'},result:{source:'groq',confidence:'validated'}}];
  const result=await waitForInfrastructureJob(async(path,body)=>{calls.push(path);return responses.shift();},'WordPress',{pollInterval:0,onProgress:job=>states.push(job.state)});
  assert.equal(result.source,'groq');assert.deepEqual(states,['queued','running','succeeded']);assert.equal(calls[0],'/api/ai-jobs');assert.equal(calls.length,3);
});

test('AI cancellation cancels only the submitted job',async()=>{
  const {waitForInfrastructureJob}=require('../lab-ui/app.js');
  const controller=new AbortController(),id='b'.repeat(48),calls=[];
  await assert.rejects(waitForInfrastructureJob(async(path)=>{calls.push(path);return path.endsWith('/cancel')?{job:{id,state:'cancelled'}}:{job:{id,state:'queued',position:1}};},'WordPress',{signal:controller.signal,onProgress:()=>controller.abort(),pollInterval:0}),error=>error.name==='AbortError');
  assert.deepEqual(calls,['/api/ai-jobs','/api/ai-jobs/'+id+'/cancel']);
});

test('AI queue refuses mismatched job IDs and propagates failed results',async()=>{
  const {waitForInfrastructureJob}=require('../lab-ui/app.js');
  const id='c'.repeat(48);let count=0;
  await assert.rejects(waitForInfrastructureJob(async(path)=>{if(path.endsWith('/cancel'))return {};return ++count===1?{job:{id,state:'running'}}:{job:{id:'d'.repeat(48),state:'succeeded'},result:{}};},'WordPress',{pollInterval:0}),/suivi/);
  await assert.rejects(waitForInfrastructureJob(async()=>({job:{id,state:'failed',error:'API temporairement indisponibles'}}),'WordPress'),/API temporairement/);
});

test('AI polling resumes the same job after a temporary network failure',async()=>{
  const {waitForInfrastructureJob}=require('../lab-ui/app.js');
  const id='e'.repeat(48),calls=[],progress=[];let polls=0;
  const result=await waitForInfrastructureJob(async(path)=>{
    calls.push(path);
    if(path==='/api/ai-jobs')return {job:{id,state:'running'}};
    if(++polls===1)throw new TypeError('Failed to fetch');
    return {job:{id,state:'succeeded'},result:{confidence:'validated'}};
  },'WordPress',{pollInterval:0,onProgress:job=>progress.push(job)});
  assert.equal(result.confidence,'validated');
  assert.equal(calls.filter(path=>path==='/api/ai-jobs').length,1);
  assert.equal(calls.some(path=>path.endsWith('/cancel')),false);
  assert.equal(progress.some(job=>job.reconnecting),true);
});

test('AI polling bounds reconnection attempts and does not retry forbidden jobs',async()=>{
  const {waitForInfrastructureJob}=require('../lab-ui/app.js');
  const id='f'.repeat(48),calls=[];
  await assert.rejects(waitForInfrastructureJob(async(path)=>{
    calls.push(path);
    if(path==='/api/ai-jobs')return {job:{id,state:'running'}};
    if(path.endsWith('/cancel'))return {};
    throw new TypeError('Failed to fetch');
  },'WordPress',{pollInterval:0}),/connexion au suivi/);
  assert.equal(calls.filter(path=>path==='/api/ai-jobs/'+id).length,6);
  assert.equal(calls.filter(path=>path.endsWith('/cancel')).length,1);
  let forbiddenPolls=0;
  await assert.rejects(waitForInfrastructureJob(async(path)=>{
    if(path==='/api/ai-jobs')return {job:{id,state:'running'}};
    if(path.endsWith('/cancel'))return {};
    forbiddenPolls++;
    throw Object.assign(new Error('Demande introuvable'),{status:404});
  },'WordPress',{pollInterval:0}),/introuvable/);
  assert.equal(forbiddenPolls,1);
});
