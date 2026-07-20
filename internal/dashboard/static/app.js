const texts = {
  en: {
    monitor:"monitor", overview:"Overview", accounts:"Accounts", projects:"Projects",
    sessions:"Sessions", tasks:"Tasks", subagents:"Subagents", services:"Services",
    privacy:"Read-only metadata view. Full transcripts and tool output are never cached.",
    profiles:"Profiles", active:"Active", approval:"Waiting approval", input:"Waiting input",
    failures:"Service failures", unmanaged:"Unmanaged", search:"Search sessions",
    healthy:"healthy", unhealthy:"unhealthy", profile:"Profile", status:"Status",
    project:"Project", updated:"Updated", tokens:"Tokens", mirror:"Mirror", conflicts:"Conflicts",
    allProfiles:"All profiles", allProjects:"All projects", allStatuses:"All statuses",
    allSources:"All sources", allModels:"All models", allArchives:"Active + archived",
    activeOnly:"Active only", archivedOnly:"Archived only", created:"Created", title:"Title",
    descending:"Descending", ascending:"Ascending", unknown:"Unavailable", daily:"Latest day",
    credits:"Credits", archived:"Archived", managed:"Managed", primary:"Primary",
    secondary:"Secondary", notSignedIn:"Not signed in", live:"live", reconnecting:"reconnecting",
    connecting:"connecting", tree:"Tree", depth:"depth", idle:"idle", ready:"ready",
    failed:"failed", error:"error", starting:"starting", authRequired:"authentication required"
  },
  "zh-CN": {
    monitor:"监控", overview:"总览", accounts:"帐号", projects:"项目", sessions:"Sessions",
    tasks:"任务", subagents:"子代理", services:"服务",
    privacy:"只读元数据视图；完整 transcript、工具输出和命令输出不会进入缓存。",
    profiles:"帐号", active:"活动任务", approval:"等待审批", input:"等待输入",
    failures:"服务异常", unmanaged:"未托管", search:"搜索 Sessions", healthy:"健康",
    unhealthy:"异常", profile:"帐号", status:"状态", project:"项目", updated:"更新时间",
    tokens:"Token", mirror:"镜像", conflicts:"冲突", allProfiles:"全部帐号",
    allProjects:"全部项目", allStatuses:"全部状态", allSources:"全部来源",
    allModels:"全部模型", allArchives:"活动和归档", activeOnly:"仅活动",
    archivedOnly:"仅归档", created:"创建时间", title:"标题", descending:"降序",
    ascending:"升序", unknown:"不可用", daily:"最近一天", credits:"Credits",
    archived:"已归档", managed:"托管", primary:"主窗口", secondary:"次窗口",
    notSignedIn:"未登录", live:"实时", reconnecting:"正在重连", connecting:"正在连接",
    tree:"层级", depth:"深度", idle:"空闲", ready:"就绪", failed:"失败", error:"错误",
    starting:"启动中", authRequired:"需要认证"
  }
};

let state = null;
let lang = (navigator.language || "en").toLowerCase().startsWith("zh") ? "zh-CN" : "en";
let view = "overview";
let sessions = {data:[], total:0, page:1, pages:1, facets:{}};
let searchTimer = null;
let sessionsRequest = 0;
const t = key => (texts[lang] || texts.en)[key] || key;
const esc = value => String(value ?? "").replace(/[&<>"']/g, char =>
  ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]));
const fmt = value => new Intl.NumberFormat(lang).format(value || 0);
const age = value => value ? new Intl.DateTimeFormat(lang, {
  dateStyle:"short", timeStyle:"short"
}).format(new Date(value)) : "—";

function init() {
  document.documentElement.lang = lang;
  document.querySelectorAll("[data-i18n]").forEach(element => {
    element.textContent = t(element.dataset.i18n);
  });
  document.querySelector("#search").placeholder = t("search");
  document.querySelector("#connection").textContent = t("connecting");
  const views = ["overview","accounts","projects","sessions","tasks","subagents"];
  document.querySelector("#nav").innerHTML = views.map(name =>
    `<button data-view="${name}">${t(name)}</button>`).join("");
  document.querySelectorAll("nav button").forEach(button => {
    button.onclick = () => show(button.dataset.view);
  });
  configureSessionControls();
  show(view);
  fetch("/api/v1/snapshot").then(response => response.json()).then(update);
  connect();
}

function configureSessionControls() {
  document.querySelector("#search").oninput = () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { sessions.page = 1; loadSessions(); }, 180);
  };
  document.querySelectorAll("[data-filter],#session-sort,#session-direction").forEach(control => {
    control.onchange = () => { sessions.page = 1; loadSessions(); };
  });
  document.querySelector("#page-prev").onclick = () => {
    if (sessions.page > 1) { sessions.page--; loadSessions(); }
  };
  document.querySelector("#page-next").onclick = () => {
    if (sessions.page < sessions.pages) { sessions.page++; loadSessions(); }
  };
  document.querySelector("#filter-archived").innerHTML =
    `<option value="">${t("allArchives")}</option><option value="false">${t("activeOnly")}</option><option value="true">${t("archivedOnly")}</option>`;
  document.querySelector("#session-sort").innerHTML =
    `<option value="updated">${t("updated")}</option><option value="created">${t("created")}</option><option value="title">${t("title")}</option><option value="tokens">${t("tokens")}</option>`;
  document.querySelector("#session-direction").innerHTML =
    `<option value="desc">${t("descending")}</option><option value="asc">${t("ascending")}</option>`;
}

function show(name) {
  view = name;
  document.querySelectorAll(".view").forEach(element =>
    element.classList.toggle("active", element.id === name));
  document.querySelectorAll("nav button").forEach(element =>
    element.classList.toggle("active", element.dataset.view === name));
  if (name === "sessions") loadSessions();
}

function connect() {
  const badge = document.querySelector("#connection");
  const events = new EventSource("/api/v1/events");
  events.onopen = () => { badge.textContent = t("live"); badge.className = "pill ok"; };
  events.addEventListener("snapshot", event => update(JSON.parse(event.data)));
  events.onerror = () => { badge.textContent = t("reconnecting"); badge.className = "pill warning"; };
}

function update(snapshot) {
  state = snapshot;
  render();
  if (view === "sessions") loadSessions();
}

function status(value) {
  const style = value === "active" || value === "healthy" || value === "idle" || value === "ready"
    ? "ok" : value === "waiting_approval" || value === "waiting_input" ||
      value === "unmanaged" || value === "starting" || value === "authentication_required"
      ? "warning" : value === "error" || value === "failed" ? "error" : "";
  const labels = {
    active:t("active"), idle:t("idle"), healthy:t("healthy"), ready:t("ready"),
    waiting_approval:t("approval"), waiting_input:t("input"), unmanaged:t("unmanaged"),
    starting:t("starting"), authentication_required:t("authRequired"), error:t("error"),
    failed:t("failed"), archived:t("archived")
  };
  return `<span class="status ${style}">${esc(labels[value] || value || "unknown")}</span>`;
}

function rows(items, fields) {
  const header = fields.map(field => `<span>${esc(field[0])}</span>`).join("");
  const body = items.map(item => `<div class="row">${
    fields.map(field => `<div>${field[1](item)}</div>`).join("")
  }</div>`).join("") || `<div class="row"><span class="muted">—</span></div>`;
  return `<div class="table"><div class="row header">${header}</div>${body}</div>`;
}

function render() {
  if (!state) return;
  const summary = state.summary || {};
  const warnings = state.warnings || [];
  const warningBox = document.querySelector("#warnings");
  warningBox.className = warnings.length ? "warnings" : "";
  warningBox.innerHTML = warnings.map(item => `<div>${esc(item)}</div>`).join("");
  document.querySelector("#summary").innerHTML = [
    [t("profiles"),summary.profiles], [t("active"),summary.active_tasks],
    [t("approval"),summary.waiting_approval], [t("input"),summary.waiting_input],
    [t("unmanaged"),summary.unmanaged], [t("failures"),summary.service_failures]
  ].map(item => `<div class="card"><span>${item[0]}</span><b>${fmt(item[1])}</b></div>`).join("");
  document.querySelector("#overview-tasks").innerHTML = taskRows((state.tasks || []).slice(0,8));
  document.querySelector("#overview-services").innerHTML = (state.services || []).map(item =>
    `<div class="metric"><span>${esc(item.profile)}</span>${status(item.healthy ? "healthy" : "error")}</div>`
  ).join("") || "—";
  document.querySelector("#account-list").innerHTML = (state.accounts || []).map(account => {
    const mcp = (account.mcp_servers || []).map(server =>
      `<div class="metric"><span>MCP · ${esc(server.name)}${
        server.error ? ` · ${esc(server.error)}` : ""
      }</span>${status(server.status || server.auth_status)}</div>`).join("");
    const latest = (account.daily_usage || []).at(-1);
    const credits = account.credits ? (account.credits.unlimited ? "∞" : account.credits.balance || "0") : "—";
    return `<div class="account-card"><div class="metric"><strong>${esc(account.profile)}</strong>${
      status(account.codex_healthy ? "healthy" : "error")
    }</div><div class="secondary">${esc(account.email || t("notSignedIn"))} · ${esc(account.plan || "—")}</div>${
      limit(account.primary,t("primary"))}${limit(account.secondary,t("secondary"))
    }<div class="metric"><span>${t("tokens")}</span><b>${fmt(account.lifetime_tokens)}</b></div>${
      `<div class="metric"><span>${t("daily")}</span><b>${latest ? fmt(latest.tokens) : "—"}</b></div>` +
      `<div class="metric"><span>${t("credits")}</span><b>${esc(credits)}</b></div>` +
      mcp}${account.error ? `<p class="error">${esc(account.error)}</p>` : ""}</div>`;
  }).join("");
  document.querySelector("#project-list").innerHTML = rows(state.projects || [], [
    [t("project"), project => `<div class="primary">${esc(project.root)}</div><div class="secondary">${esc(project.git_branch || project.binding_source || "")}</div>`],
    [t("profile"), project => esc(project.profile)],
    [t("sessions"), project => `${fmt(project.sessions)}<div class="secondary">${t("active")} ${fmt(project.active_tasks)} · ${
      t("tokens")} ${project.token_sessions ? `${fmt(project.tokens)} / ${fmt(project.token_sessions)}` : t("unknown")
    }</div>`],
    [t("mirror"), project => project.mirror?.validation_error ? status("error") :
      `${fmt((project.mirror?.sessions || 0) + (project.mirror?.archived_sessions || 0))} · ${t("conflicts")} ${fmt(project.mirror?.pending?.conflicts?.length)}`]
  ]);
  document.querySelector("#task-list").innerHTML = taskRows(state.tasks || []);
  document.querySelector("#subagent-list").innerHTML = rows(state.subagents || [], [
    ["ID", agent => `<div class="primary">${esc(agent.nickname || agent.id)}</div><div class="secondary">${esc(agent.role || agent.path || agent.project || agent.parent_id)}</div>`],
    [t("profile"), agent => esc(agent.profile)],
    [t("status"), agent => status(agent.status)],
    [t("tree"), agent => agent.cycle ? status("cycle") : agent.orphan ? status("orphan") :
      `${t("depth")} ${fmt(agent.depth)} · ${esc(agent.task_id || agent.parent_id || "—")}`]
  ]);
}

async function loadSessions() {
  const request = ++sessionsRequest;
  const params = new URLSearchParams({
    page:String(sessions.page || 1), page_size:"50",
    q:document.querySelector("#search").value,
    sort:document.querySelector("#session-sort").value,
    direction:document.querySelector("#session-direction").value
  });
  document.querySelectorAll("[data-filter]").forEach(control => {
    if (control.value) params.set(control.dataset.filter, control.value);
  });
  try {
    const response = await fetch(`/api/v1/sessions?${params}`);
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const page = await response.json();
    if (request !== sessionsRequest) return;
    sessions = page;
    populateFacets(page.facets || {});
    renderSessions();
  } catch (error) {
    if (request === sessionsRequest) {
      document.querySelector("#session-list").innerHTML = `<p class="error">${esc(error.message)}</p>`;
    }
  }
}

function populateFacets(facets) {
  setOptions("filter-profile", t("allProfiles"), facets.profiles);
  setOptions("filter-project", t("allProjects"), facets.projects);
  setOptions("filter-status", t("allStatuses"), facets.statuses);
  setOptions("filter-source", t("allSources"), facets.sources);
  setOptions("filter-model", t("allModels"), facets.models);
}

function setOptions(id, label, values = []) {
  const select = document.querySelector(`#${id}`);
  const current = select.value;
  select.innerHTML = `<option value="">${esc(label)}</option>` +
    values.map(value => `<option value="${esc(value)}">${esc(value)}</option>`).join("");
  if (values.includes(current)) select.value = current;
}

function renderSessions() {
  document.querySelector("#session-list").innerHTML = rows(sessions.data || [], [
    [t("sessions"), item => `<div class="primary">${esc(item.title)}</div><div class="secondary">ID · ${esc(item.id)}${
      item.preview ? ` · ${esc(item.preview)}` : ""
    }</div>`],
    [t("profile"), item => `${esc(item.profile)}<div class="secondary">${esc([
      item.model || item.source, item.git_branch, item.archived ? t("archived") : ""
    ].filter(Boolean).join(" · ") || "—")}</div>`],
    [t("status"), item => status(item.status)],
    [t("updated"), item => `${age(item.updated_at)}<div class="secondary">${t("created")} ${age(item.created_at)} · ${
      item.token_known ? fmt(item.tokens) : t("unknown")
    } ${t("tokens")}</div>`]
  ]);
  document.querySelector("#page-info").textContent =
    `${fmt(sessions.page)} / ${fmt(sessions.pages)} · ${fmt(sessions.total)}`;
  document.querySelector("#page-prev").disabled = sessions.page <= 1;
  document.querySelector("#page-next").disabled = sessions.page >= sessions.pages;
}

function limit(value,label) {
  if (!value) return "";
  const percent = Math.max(0,Math.min(100,value.usedPercent || 0));
  return `<div class="metric"><span>${label}</span><span>${percent}%</span></div><progress class="bar" max="100" value="${percent}">${percent}%</progress>`;
}

function taskRows(items) {
  return rows(items, [
    [t("tasks"), item => `<div class="primary">${esc(item.title)}</div><div class="secondary">${esc(item.project || "")}</div>`],
    [t("profile"), item => esc(item.profile)],
    [t("status"), item => `${status(item.status)}<div class="secondary">${item.managed ? t("managed") : t("unmanaged")}</div>`],
    [t("updated"), item => age(item.last_activity)]
  ]);
}

init();
