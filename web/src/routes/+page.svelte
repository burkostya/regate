<script lang="ts">
  import { onMount } from 'svelte';
  import FlowCanvas from '$lib/FlowCanvas.svelte';
  import { api, setToken, type Catalog, type Pipeline, type Draft, type EdgeDef } from '$lib/api';
  import { pipelineToHCL, createNode } from '$lib/hcl';

  type Tab = 'Overview' | 'Upstreams' | 'Surfaces' | 'Rules' | 'Pipelines' | 'HCL' | 'Revisions' | 'Traces' | 'Effects';
  let tab = $state<Tab>('Pipelines');
  let credentials = $state('');
  let authenticated = $state(false);
  let status = $state<Record<string, any> | null>(null);
  let catalog = $state<Catalog | null>(null);
  let selected = $state('');
  let activePipeline = $state<Pipeline | null>(null);
  let selectedNode = $state('');
  let nodeJson = $state('');
  let positions = $state.raw<Record<string, {x: number; y: number}>>({});
  let dirty = $state(false);
  let draft = $state<Draft | null>(null);
  let editorText = $state('');
  let testArguments = $state('{"customer_id":"C123"}');
  let resultText = $state('');
  let message = $state('Enter the admin token to connect to Regate.');
  let error = $state('');
  let busy = $state(false);
  let revisions = $state<any[]>([]);
  let traces = $state<any[]>([]);
  let effects = $state<any[]>([]);
  let insight = $state('');
  let upstreamName = $state('');
  let discovered = $state<any[]>([]);
  const tabs: Tab[] = ['Overview', 'Upstreams', 'Surfaces', 'Rules', 'Pipelines', 'HCL', 'Revisions', 'Traces', 'Effects'];
  const kinds = ['input','output','transform','filter','validate','condition','parallel','join','mcp.call','hook.call','http.call','policy.check','mask.pii','suspend'];

  async function task(fn: () => Promise<void>) {
    error = ''; busy = true;
    try { await fn(); } catch (e) { error = e instanceof Error ? e.message : String(e); }
    finally { busy = false; }
  }
  async function login() {
    setToken(credentials);
    await task(async () => {
      status = await api('status');
      authenticated = true;
      await refresh();
      message = `Connected to Regate ${status?.revision?.slice(0, 8) ?? ''}`;
    });
  }
  async function refresh() {
    [catalog, status, revisions, traces, effects] = await Promise.all([api('catalog'),api('status'),api('revisions'),api('traces'),api('effects')]);
    if (catalog && (!selected || !catalog.pipelines[selected])) selected = Object.keys(catalog.pipelines)[0] ?? '';
    await selectPipeline(selected);
  }
  async function selectPipeline(name: string) {
    if (!catalog?.pipelines[name]) return;
    selected = name;
    activePipeline = structuredClone(catalog.pipelines[name]);
    dirty = false; selectedNode = '';
    positions = await api(`layout/${encodeURIComponent(name)}`).catch(() => ({}));
  }
  async function ensureDraft(): Promise<Draft> {
    if (draft) return draft;
    const created = await api<Draft>('drafts', 'POST', {});
    draft = created;
    editorText = created.source;
    return created;
  }
  async function createDraft() {
    const created = await api<Draft>('drafts', 'POST', {});
    draft = created;
    editorText = created.source;
    message = `Draft ${created.id.slice(0,8)} created`;
  }
  async function saveHCL() {
    const changedSource = editorText;
    const d = await ensureDraft();
    const source = changedSource.trim() ? changedSource : d.source;
    draft = await api('draft-update','POST',{draft_id:d.id,source});
    editorText = source;
    message = 'HCL validated and saved in draft. Active configuration unchanged.';
  }
  async function saveGraph() {
    if (!activePipeline) return;
    const d = await ensureDraft();
    const updated = await api<Draft>('graph-patch','POST',{draft_id:d.id,pipeline:activePipeline.name,source:pipelineToHCL(activePipeline)});
    draft = updated;
    editorText = updated.source;
    dirty = false;
    message = 'Pipeline changed in draft; other top-level HCL blocks preserved.';
  }
  async function saveLayout() {
    await api(`layout/${encodeURIComponent(selected)}`,'PUT',positions);
    message = 'Node positions saved independently from HCL.';
  }
  async function validate() {
    const source = draft ? editorText : (await api<{source:string}>('config')).source;
    const data = await api('validate','POST',{source});
    insight = JSON.stringify(data,null,2);
    tab = 'HCL';
    if (data.valid) message = 'Configuration validated'; else error = data.error;
  }
  async function diff() {
    const d = await ensureDraft();
    insight = JSON.stringify(await api('diff','POST',{draft_id:d.id}),null,2);
    tab='HCL';
  }
  async function activate() {
    if (!draft) throw new Error('Create and validate a draft first');
    const result = await api('validate','POST',{draft_id:draft.id});
    if (!result.valid) throw new Error(result.error);
    if (!confirm('Activate this configuration revision? Existing calls continue on the previous revision.')) return;
    await api('activate','POST',{draft_id:draft.id});
    draft = null; editorText = ''; insight='';
    await refresh(); message = 'New revision activated.';
  }
  async function rollback(id: string) {
    if (!confirm(`Restore revision ${id.slice(0,12)} as a new activation?`)) return;
    await api('rollback','POST',{revision:id});
    draft = null; await refresh(); message = 'Rollback activated.';
  }
  async function runTool(live: boolean) {
    if (!catalog) return;
    const available = Object.entries(catalog.surfaces).flatMap(([surface,value]) => (value.tools ?? []).map((t:any) => ({surface,t})));
    const match = available.find(({t}) => t.pipeline === selected);
    if (!match) throw new Error('No surface tool exposes this pipeline');
    if (live && !confirm('Perform LIVE execution? External tools can have side effects.')) return;
    const args = JSON.parse(testArguments);
    const endpoint = live ? 'run' : 'test';
    const output = await api(endpoint,'POST',{draft_id:live?undefined:draft?.id,surface:match.surface,tool:match.t.name,arguments:args});
    resultText = JSON.stringify(output,null,2);
  }
  function selectNode(id: string) {
    selectedNode = id;
    nodeJson = activePipeline?.nodes[id] ? JSON.stringify(activePipeline.nodes[id].attrs,null,2) : '';
  }
  function updateNode() {
    if (!activePipeline || !selectedNode) return;
    const attrs = JSON.parse(nodeJson);
    if (!attrs.kind || typeof attrs.kind !== 'string') throw new Error('Node needs a kind');
    activePipeline = {...activePipeline,nodes:{...activePipeline.nodes,[selectedNode]:{name:selectedNode,kind:attrs.kind,attrs}}};
    dirty = true;
  }
  function removeNode() {
    if (!activePipeline || !selectedNode || selectedNode === activePipeline.entry) throw new Error('Cannot remove pipeline entry');
    const nodes = {...activePipeline.nodes};delete nodes[selectedNode];
    activePipeline = {...activePipeline,nodes,edges:activePipeline.edges.filter(e=>e.from!==selectedNode&&e.to!==selectedNode)};
    selectedNode = ''; dirty = true;
  }
  function addNode(kind: string) {
    if (!activePipeline) return;
    activePipeline = createNode(activePipeline,kind);dirty=true;
    selectNode(Object.keys(activePipeline.nodes).at(-1)!);
  }
  function onGraphChange(edges:EdgeDef[], newPositions:Record<string,{x:number;y:number}>) {
    if (!activePipeline)return;
    positions = newPositions;
    if (JSON.stringify(activePipeline.edges) !== JSON.stringify(edges)) {
      activePipeline = {...activePipeline,edges};dirty = true;
    }
  }
  async function inspectUpstream(name:string) {
    upstreamName = name;
    const d = await api<{items:any[]}>('discover','POST',{name});
    discovered = d.items ?? [];
  }
  onMount(() => {});
</script>

<svelte:head>
  <title>Regate Console · MCP Gateway</title>
  <meta name="description" content="Regate MCP gateway — configure, inspect and compose tools and pipelines." />
</svelte:head>

<div class="app">
  <aside class="sidebar">
    <div class="branding"><div class="brand-icon">R</div><div><strong>REGATE</strong><small>MCP CONTROL PLANE</small></div></div>
    <div class="navtitle">WORKSPACE</div>
    <nav aria-label="Main navigation">
      {#each tabs as item}
        <button type="button" class:active={tab===item} onclick={() => {tab=item;error='';}}>{item}</button>
      {/each}
    </nav>
    <div class="spacer"></div>
    {#if status}
      <div class="small-panel"><div class="statusdot"></div><span>Gateway online</span><code>{status.revision.slice(0,8)}</code></div>
    {/if}
    <div class="footer">HTTP ONLY · GO RUNTIME</div>
  </aside>
  <main>
    <header class="topbar"><div class="breadcrumbs">Workspace <span>/</span> <b>{tab}</b> {#if selected && tab==='Pipelines'}<span>/</span> {selected}{/if}</div>
      <div class="actions">{#if draft}<span class="draft-badge">Draft {draft.id.slice(0,8)}</span>{/if}<button class="secondary" disabled={!authenticated||busy} onclick={() => task(refresh)}>Refresh</button><button class="primary" disabled={!authenticated||busy} onclick={() => task(createDraft)}>New draft</button></div>
    </header>
    {#if !authenticated}
      <section class="login-panel"><div class="login-icon">R</div><h1>Connect to Regate</h1><p>Authenticate against the management API. Your token stays in this browser tab's memory.</p><form onsubmit={(event) => {event.preventDefault();login();}}><label for="token">Admin bearer token</label><input id="token" type="password" autocomplete="off" bind:value={credentials} placeholder="REGATE_ADMIN_TOKEN" required/><button class="primary" type="submit" disabled={busy}>Connect</button></form></section>
    {:else}
      <div class="workspace">
        {#if tab==='Overview'}
          <div class="page-title"><div><p class="eyebrow">SYSTEM HEALTH</p><h1>Gateway overview</h1></div><button class="secondary" onclick={() => task(refresh)}>Refresh status</button></div>
          <div class="metrics"><div class="metric"><span>Upstreams</span><b>{status?.upstreams ?? 0}</b></div><div class="metric"><span>Published surfaces</span><b>{status?.surfaces ?? 0}</b></div><div class="metric"><span>Pipelines</span><b>{status?.pipelines ?? 0}</b></div><div class="metric"><span>Hooks</span><b>{status?.hooks ?? 0}</b></div></div>
          <section class="pane"><h2>Active revision</h2><code>{status?.revision}</code><p>HCL is the execution source; layout positions are stored separately. Existing calls retain their revision on activation.</p></section>
        {:else if tab==='Upstreams'}
          <div class="page-title"><div><p class="eyebrow">DISCOVERY</p><h1>Upstream MCP servers</h1></div></div>
          <div class="split-pane"><div class="pane"><h2>Connections</h2>{#each Object.entries(catalog?.upstreams ?? {}) as [name,u]}<button class="list-row" class:selected={upstreamName===name} onclick={() => task(() => inspectUpstream(name))}><strong>{name}</strong><span>{(u as any).url}</span></button>{/each}</div>
            <div class="pane"><h2>{upstreamName || 'Select an upstream'}</h2><p>Refresh tools directly from the upstream MCP endpoint.</p>{#each discovered as t}<div class="tool"><strong>{t.name}</strong><p>{t.description ?? 'No description'}</p><details><summary>Input Schema</summary><pre>{JSON.stringify(t.inputSchema,null,2)}</pre></details></div>{/each}</div></div>
        {:else if tab==='Surfaces'}
          <div class="page-title"><div><p class="eyebrow">PUBLIC INTERFACES</p><h1>MCP surfaces</h1></div></div>
          <div class="cards">{#each Object.entries(catalog?.surfaces ?? {}) as [name,s]}<section class="pane"><p class="eyebrow">VIRTUAL SERVER</p><h2>{name}</h2><code>{s.path}</code><p>{s.tools?.length??0} tools · {s.resources?.length??0} resources · {s.prompts?.length??0} prompts</p>{#each s.tools ?? [] as t}<div class="tool"><strong>{t.name}</strong><span>{t.pipeline ? `pipeline: ${t.pipeline}` : `source: ${t.source}`}</span></div>{/each}</section>{/each}</div>
        {:else if tab==='Rules'}
          <div class="page-title"><div><p class="eyebrow">GUARDS &amp; TRANSFORMS</p><h1>Hooks and policies</h1></div><button class="secondary" onclick={() => { tab='HCL'; task(createDraft); }}>Edit in HCL</button></div>
          <div class="split-pane"><div class="pane"><h2>Hooks ({Object.keys(catalog?.hooks??{}).length})</h2>{#each Object.entries(catalog?.hooks??{}) as [name, h]}<div class="tool"><strong>{name}</strong><p>{(h as any).transport} · {(h as any).contract}</p><code>{(h as any).url || `${(h as any).upstream}.${(h as any).tool}`}</code><p>Timeout {(h as any).timeout_ms ?? 'default'} ms · failure: {(h as any).on_error || 'deny'}</p></div>{/each}</div>
            <div class="pane"><h2>Decision policies</h2>{#each Object.entries(catalog?.policies??{}) as [name, p]}<div class="tool"><strong>{name}</strong><p>Decision hook: {(p as any).decision_hook || 'none'}</p><p>Scope: {(p as any).scope || 'unspecified'}</p></div>{/each}<div class="divider"></div><h2>Interceptors</h2>{#each catalog?.interceptors??[] as i}<div class="tool"><strong>{(i as any).name}</strong><p>{(i as any).stage} · hook {(i as any).hook}</p><p>Scope: {(i as any).surface || 'all surfaces'} / {(i as any).tool || 'all tools'}</p></div>{/each}</div>
          </div>
        {:else if tab==='Pipelines'}
          <div class="page-title"><div><p class="eyebrow">PIPELINE DESIGNER</p><h1>Execution graph</h1></div><div class="actions"><select aria-label="Choose pipeline" bind:value={selected} onchange={() => task(() => selectPipeline(selected))}>{#each Object.keys(catalog?.pipelines ?? {}) as name}<option value={name}>{name}</option>{/each}</select>{#if dirty}<span class="unsaved">Unsaved graph edits</span>{/if}</div></div>
          {#if activePipeline}
            <div class="editor-grid"><div class="palette"><h3>Pipeline nodes</h3><p>Add a processing step</p><div class="palette-items">{#each kinds as kind}<button title={'Add '+kind} onclick={() => addNode(kind)}><span class="plus">+</span>{kind}</button>{/each}</div><div class="palette-divider"></div><h3>Existing nodes</h3>{#each Object.values(activePipeline.nodes) as n}<button class:selected={selectedNode===n.name} class="node-item" onclick={() => selectNode(n.name)}>{n.name}<small>{n.kind}</small></button>{/each}</div>
              <div class="canvas"><FlowCanvas {positions} pipeline={activePipeline} onchange={onGraphChange} onselect={selectNode}/></div>
              <aside class="inspector"><h3>Inspector</h3>{#if selectedNode}<div class="field"><span class="field-label">Node</span><code>{selectedNode}</code></div><div class="field"><label for="node-attributes">Attributes (JSON)</label><textarea id="node-attributes" spellcheck="false" rows="13" bind:value={nodeJson}></textarea></div><div class="inspector-actions"><button class="primary" onclick={() => task(async()=>updateNode())}>Apply</button><button class="danger" onclick={() => task(async()=>removeNode())}>Delete</button></div>{:else}<p>Select a node in the graph or node list to edit its attributes.</p>{/if}<div class="divider"></div><h3>Pipeline details</h3><p>Entry: <code>{activePipeline.entry}</code></p><p>Timeout: {activePipeline.timeout_ms} ms</p><p>Concurrency: {activePipeline.concurrency}</p></aside></div>
            <div class="toolbar"><div class="actions"><button class="primary" disabled={busy} onclick={() => task(saveGraph)}>Save graph to draft</button><button class="secondary" onclick={() => task(saveLayout)}>Save layout</button><button class="secondary" onclick={() => task(validate)}>Validate HCL</button></div><div class="actions"><button class="secondary" onclick={() => task(() => runTool(false))}>Dry-run</button><button class="secondary" onclick={() => task(() => runTool(true))}>Live run</button></div></div>
            <div class="split-pane bottom"><div class="pane"><h3>Tool arguments</h3><textarea rows="5" spellcheck="false" bind:value={testArguments}></textarea><p>Dry-run requires mocks for external call nodes and doesn't contact upstream services.</p></div><div class="pane"><h3>Last execution</h3><pre class="output">{resultText || 'No execution yet.'}</pre></div></div>
          {/if}
        {:else if tab==='HCL'}
          <div class="page-title"><div><p class="eyebrow">DECLARATIVE SOURCE</p><h1>HCL editor</h1></div><div class="actions"><button class="secondary" onclick={() => task(async()=>{const r=await api<{source:string}>('config');editorText=r.source;})}>Load active</button><button class="secondary" onclick={() => task(diff)}>Diff</button><button class="secondary" onclick={() => task(validate)}>Validate</button><button class="primary" onclick={() => task(saveHCL)}>Save draft</button><button class="primary" onclick={() => task(activate)}>Activate</button></div></div>
          <div class="pane"><label for="hcl-source">Regate configuration {draft ? '· Draft '+draft.id.slice(0,8) : '· Active preview'}</label><textarea id="hcl-source" class="hcl-editor" spellcheck="false" bind:value={editorText} placeholder="Create a draft or load the active HCL source..."></textarea></div>
          {#if insight}<div class="pane"><h3>Validation / Impact</h3><pre class="output">{insight}</pre></div>{/if}
        {:else if tab==='Revisions'}
          <div class="page-title"><div><p class="eyebrow">HISTORY</p><h1>Immutable revisions</h1></div></div>
          <section class="pane">{#each [...revisions].reverse() as rev}<div class="revision"><div><strong><code>{rev.id}</code></strong><p>{new Date(rev.activated).toLocaleString()} {rev.parent ? `· from ${rev.parent.slice(0,8)}` : ''}</p></div><div class="actions">{#if rev.id===status?.revision}<span class="live">ACTIVE</span>{:else}<button class="secondary" onclick={() => task(() => rollback(rev.id))}>Restore</button>{/if}</div></div>{/each}</section>
        {:else if tab==='Traces'}
          <div class="page-title"><div><p class="eyebrow">OBSERVABILITY</p><h1>Execution traces</h1></div><button class="secondary" onclick={() => task(refresh)}>Refresh</button></div>
          <section class="pane"><table><thead><tr><th>Time</th><th>Run</th><th>Node</th><th>State</th><th>Duration</th></tr></thead><tbody>{#each traces.slice(0,100) as trace}<tr><td>{new Date(trace.at).toLocaleTimeString()}</td><td><code>{trace.run_id.slice(0,8)}</code></td><td>{trace.node || trace.tool || '—'}</td><td>{trace.status}</td><td>{trace.duration_ms??'—'} ms</td></tr>{/each}</tbody></table></section>
        {:else if tab==='Effects'}
          <div class="page-title"><div><p class="eyebrow">EXTERNAL SIDE EFFECTS</p><h1>Effect journal</h1></div><button class="secondary" onclick={() => task(refresh)}>Refresh</button></div>
          <section class="pane"><p>Uncertain external effects need read-back reconciliation. Do not blindly retry writes.</p><table><thead><tr><th>Time</th><th>Run</th><th>Operation</th><th>Status</th><th>Reason</th></tr></thead><tbody>{#each effects.slice(0,100) as effect}<tr><td>{new Date(effect.at).toLocaleTimeString()}</td><td><code>{effect.run_id?.slice(0,8)??'—'}</code></td><td>{effect.operation}</td><td><strong>{effect.status}</strong></td><td>{effect.reason ?? ''}</td></tr>{/each}</tbody></table></section>
        {/if}
      </div>
    {/if}
    {#if error}<div class="toast error" role="alert"><strong>Error</strong> {error}<button onclick={()=>error=''}>×</button></div>{:else if message && authenticated}<div class="toast" role="status">{message}<button onclick={()=>message=''}>×</button></div>{/if}
  </main>
</div>

<style>
  :global(*){box-sizing:border-box} :global(html){font-family:Inter,ui-sans-serif,system-ui,Arial;color:#e9eff7;background:#111b29} :global(body){margin:0} :global(button), :global(input), :global(textarea), :global(select){font:inherit} :global(button){cursor:pointer} :global(button:disabled){opacity:.5;cursor:not-allowed}
  .app{min-height:100vh;display:flex}.sidebar{width:224px;flex-shrink:0;border-right:1px solid #26344a;background:#142031;display:flex;flex-direction:column;min-height:100vh;padding:22px 14px}.branding{display:flex;align-items:center;gap:12px;padding:0 8px 31px}.brand-icon,.login-icon{height:38px;width:38px;border-radius:11px;background:#78b5df;color:#102032;display:grid;place-items:center;font-weight:900;font-size:24px}.branding strong{display:block;font-size:17px;letter-spacing:.13em}.branding small{color:#8191a6;font-size:9px;letter-spacing:.12em}.navtitle,.eyebrow{font-size:10px;font-weight:800;letter-spacing:.16em;color:#8194b0}.navtitle{margin:0 14px 10px}.sidebar nav{display:grid;gap:4px}.sidebar nav button{text-align:left;border:0;background:transparent;color:#a3b2c8;padding:12px 15px;border-radius:8px;font-size:13px}.sidebar nav button:hover,.sidebar nav button.active{background:#253a51;color:#fff}.sidebar nav button.active{border-left:3px solid #79b7e4}.spacer{flex:1}.small-panel{display:flex;align-items:center;gap:7px;font-size:11px;padding:14px 8px;color:#b5c7d7}.small-panel code{font-size:10px;color:#8296ad}.statusdot{width:7px;height:7px;border-radius:50%;background:#56c89a}.footer{font-size:9px;letter-spacing:.13em;color:#65809b;padding:14px 8px}
  main{min-width:0;flex:1}.topbar{height:69px;padding:0 27px;border-bottom:1px solid #26344a;display:flex;align-items:center;justify-content:space-between;background:#162334}.breadcrumbs{font-size:12px;color:#93a5bd;display:flex;align-items:center;gap:12px}.breadcrumbs span{color:#536b82}.breadcrumbs b{color:#e9eff7}.actions{display:flex;align-items:center;gap:8px;flex-wrap:wrap}button.primary,button.secondary,button.danger{padding:9px 13px;border-radius:7px;font-size:12px;font-weight:650}button.primary{background:#76b6e2;color:#142034;border:1px solid #76b6e2}button.secondary{border:1px solid #40526c;background:#243247;color:#c7d5e7}button.danger{background:#483044;color:#fdb7b7;border:1px solid #71475b}.draft-badge,.live{font-size:11px;color:#acd5ff;background:#243d57;border-radius:20px;padding:7px 9px}.workspace{padding:28px;display:grid;gap:20px;max-width:1700px;margin:auto}.page-title{display:flex;justify-content:space-between;align-items:center;gap:16px}.page-title h1{margin:6px 0;font-size:23px;font-weight:670}.eyebrow{margin:0}.pane{border:1px solid #314158;border-radius:11px;background:#192639;padding:20px;min-width:0}.pane h2,.pane h3{margin:0 0 16px;font-size:15px}.pane p,.inspector p,.palette p{color:#91a4bb;font-size:12px;line-height:1.5}.pane code,code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;color:#9bcefa;font-size:12px}.metrics{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:15px}.metric{background:#1b2a3e;border:1px solid #34465f;border-radius:10px;padding:20px}.metric span{display:block;color:#9aacc2;font-size:12px}.metric b{display:block;font-size:29px;margin-top:9px}.split-pane{display:grid;grid-template-columns:1fr 1fr;gap:18px}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:17px}.tool{margin:12px 0;border-top:1px solid #33445c;padding-top:12px;display:grid;gap:7px;font-size:12px}.tool span{color:#9baec4}.tool p{margin:0}.list-row{display:grid;text-align:left;gap:8px;width:100%;border:1px solid #34445d;background:#202e43;color:#e3eaf4;border-radius:8px;padding:12px;margin:8px 0}.list-row.selected{border-color:#83bce8}.list-row span{font-size:11px;color:#9aacc2;overflow-wrap:anywhere}.editor-grid{display:grid;grid-template-columns:210px minmax(350px,1fr) 245px;gap:12px;height:520px}.palette,.inspector{border:1px solid #314158;border-radius:10px;background:#192639;padding:15px;overflow:auto}.palette h3,.inspector h3{font-size:13px;margin:0 0 12px}.palette p{margin:0 0 11px}.palette-items{display:grid;grid-template-columns:1fr;gap:5px}.palette button{border:1px solid #34465c;background:#233349;border-radius:6px;color:#d7e2ef;text-align:left;padding:7px 8px;font-size:11px;display:flex;gap:8px;align-items:center}.palette button:hover,.palette button.selected{border-color:#89bfe7}.plus{font-size:14px;color:#89bfe7}.palette-divider,.divider{border-top:1px solid #314158;margin:16px 0}.palette .node-item{display:grid;gap:3px;width:100%;margin-bottom:4px}.node-item small{color:#8da9c3}.canvas{min-width:0;min-height:0}.inspector .field{display:grid;gap:6px;margin:12px 0}.field label,.pane>label{font-size:11px;color:#a7bad3}.field textarea,.pane textarea,.login-panel input{width:100%;border:1px solid #3e5068;background:#101b2b;color:#e3eaf3;border-radius:7px;padding:11px;font-size:12px}.field textarea,.pane textarea{font-family:ui-monospace,Consolas,monospace;line-height:1.55;resize:vertical}.inspector-actions{display:flex;gap:8px}.toolbar{display:flex;justify-content:space-between;align-items:center;gap:14px}.unsaved{color:#f6cd89;font-size:11px}.bottom{margin-top:0}.pane .output{font-size:11px;overflow:auto;max-height:250px;white-space:pre-wrap;color:#a8cdec}.hcl-editor{min-height:520px;resize:vertical;line-height:1.6!important;margin-top:10px}.revision{display:flex;justify-content:space-between;align-items:center;border-bottom:1px solid #33445c;padding:14px 2px}.revision p{margin:5px 0 0}table{border-collapse:collapse;width:100%;text-align:left;font-size:12px}th,td{padding:11px;border-bottom:1px solid #34465d}th{color:#91a4bd}.toast{position:fixed;right:20px;bottom:20px;background:#26415a;border:1px solid #466988;color:#d6eaff;border-radius:9px;padding:12px 15px;font-size:12px;display:flex;gap:12px;max-width:460px;z-index:200}.toast.error{background:#452a36;border-color:#8b4356}.toast button{background:none;border:0;color:#fff;font-size:18px}.login-panel{max-width:430px;margin:100px auto;padding:34px;background:#1b2a3e;border:1px solid #33455e;border-radius:14px}.login-panel h1{margin:18px 0 7px}.login-panel p{color:#9aacc2;font-size:13px;line-height:1.6}.login-panel form{display:grid;gap:12px;margin-top:22px}.login-panel label{font-size:12px;color:#afc4d9}.login-panel input{height:42px}.login-panel button{height:40px}
  @media(max-width:1150px){.editor-grid{grid-template-columns:150px minmax(300px,1fr);height:620px}.inspector{grid-column:1/-1;min-height:190px}.toolbar{margin-top:200px}.sidebar{width:180px}.metrics{grid-template-columns:repeat(2,1fr)}}
  @media(max-width:750px){.app{display:block}.sidebar{width:100%;min-height:auto;padding:12px}.branding{padding:0}.navtitle,.footer,.small-panel,.spacer{display:none}.sidebar nav{display:flex;overflow:auto}.sidebar nav button{white-space:nowrap}.topbar{padding:10px;height:auto;gap:8px;flex-wrap:wrap}.workspace{padding:15px}.editor-grid{display:block;height:auto}.palette,.canvas,.inspector{min-height:200px;margin-bottom:12px}.canvas{height:430px}.toolbar{margin:0;flex-wrap:wrap}.metrics,.split-pane{grid-template-columns:1fr}}
</style>
