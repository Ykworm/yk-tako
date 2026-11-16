(() => {
  "use strict";

  const NS = "http://www.w3.org/2000/svg";
  // 代码落点的链接用 GitHub 绝对地址，不用 "../../yk-tako-eino/" 这种相对路径——
  // 相对路径在 Pages 以 doc/ 为根时会指到站点外面去，绝对地址在任何部署位置都成立。
  const CODE_BASE = "https://github.com/Ykworm/yk-tako/blob/main/yk-tako-eino/";
  const poster = new URLSearchParams(location.search).has("poster");
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  const body = document.body;
  const svg = document.getElementById("diagram");
  const detail = document.getElementById("detail");
  const mobileView = document.getElementById("mobile-view");
  const content = document.getElementById("content-sections");
  const search = document.getElementById("search");
  const searchStatus = document.getElementById("search-status");
  const filters = document.getElementById("filters");
  const playButton = document.getElementById("play");
  const previousButton = document.getElementById("previous-step");
  const nextButton = document.getElementById("next-step");
  const zoomValue = document.getElementById("zoom-value");

  let data;
  try {
    data = JSON.parse(document.getElementById("page-data").textContent);
  } catch (error) {
    const message = "本页数据无法解析。请改用下方真源文档，或重新取得完整文件。";
    if (detail) detail.innerHTML = `<div class="error-state" role="alert">${message}</div>`;
    if (mobileView) mobileView.innerHTML = `<div class="error-state" role="alert">${message}</div>`;
    return;
  }

  const state = {
    selected: null,
    step: -1,
    playing: false,
    timer: null,
    raf: null,
    scale: 1,
    tx: 0,
    ty: 0,
    flows: new Set(Object.keys(data.flows || {})),
    query: ""
  };
  const nodes = new Map();
  const edges = new Map();
  let viewBox = poster ? (data.posterViewBox || data.viewBox) : data.viewBox;

  const escapeHTML = value => String(value ?? "").replace(/[&<>"']/g, char => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  })[char]);
  const inline = value => escapeHTML(value).replace(/`([^`]+)`/g, "<code>$1</code>");
  const localLinks = (items, prefix) => (items || []).map(item => {
    const path = typeof item === "string" ? item : item.path;
    const label = typeof item === "string" ? item : (item.label || item.path);
    return `<a href="${escapeHTML(prefix + path)}">${escapeHTML(label)}</a>`;
  }).join("");
  const svgElement = (tag, attributes = {}, text) => {
    const element = document.createElementNS(NS, tag);
    Object.entries(attributes).forEach(([key, value]) => element.setAttribute(key, String(value)));
    if (text !== undefined) element.textContent = text;
    return element;
  };
  const fitNodeText = (value, node, fontSize) => {
    const text = String(value || "");
    const max = Math.max(8, Math.floor((node.w - 28) / (fontSize * 0.62)));
    return text.length > max ? `${text.slice(0, Math.max(7, max - 1))}…` : text;
  };
  const stopPulse = () => {
    if (state.raf) cancelAnimationFrame(state.raf);
    state.raf = null;
    const pulse = document.getElementById("pulse-dot");
    if (pulse) pulse.style.opacity = "0";
  };
  const stopPlaying = () => {
    state.playing = false;
    if (state.timer) clearTimeout(state.timer);
    state.timer = null;
    stopPulse();
    if (playButton) {
      playButton.textContent = "播放链路";
      playButton.setAttribute("aria-pressed", "false");
    }
  };

  const updateTransform = () => {
    const root = document.getElementById("graph-root");
    if (root) root.setAttribute("transform", `translate(${state.tx} ${state.ty}) scale(${state.scale})`);
    if (zoomValue) zoomValue.textContent = `${Math.round(state.scale * 100)}%`;
  };
  const zoomAt = (x, y, factor) => {
    const next = Math.max(0.45, Math.min(2.8, state.scale * factor));
    const ratio = next / state.scale;
    state.tx = x - (x - state.tx) * ratio;
    state.ty = y - (y - state.ty) * ratio;
    state.scale = next;
    updateTransform();
  };
  const svgPoint = event => {
    const point = svg.createSVGPoint();
    point.x = event.clientX;
    point.y = event.clientY;
    const matrix = svg.getScreenCTM();
    return matrix ? point.matrixTransform(matrix.inverse()) : { x: viewBox[2] / 2, y: viewBox[3] / 2 };
  };

  const anchor = (node, side, fraction = 0.5) => {
    if (side === "top") return { x: node.x + node.w * fraction, y: node.y };
    if (side === "bottom") return { x: node.x + node.w * fraction, y: node.y + node.h };
    if (side === "left") return { x: node.x, y: node.y + node.h * fraction };
    return { x: node.x + node.w, y: node.y + node.h * fraction };
  };
  const geometry = edge => {
    const from = data.nodes.find(node => node.id === edge.from);
    const to = data.nodes.find(node => node.id === edge.to);
    let fromSide = edge.fromSide;
    let toSide = edge.toSide;
    if (!fromSide || !toSide) {
      const dx = (to.x + to.w / 2) - (from.x + from.w / 2);
      const dy = (to.y + to.h / 2) - (from.y + from.h / 2);
      if (Math.abs(dy) > Math.abs(dx) * 0.65) {
        fromSide = dy >= 0 ? "bottom" : "top";
        toSide = dy >= 0 ? "top" : "bottom";
      } else {
        fromSide = dx >= 0 ? "right" : "left";
        toSide = dx >= 0 ? "left" : "right";
      }
    }
    const a = anchor(from, fromSide, edge.fromFraction);
    const b = anchor(to, toSide, edge.toFraction);
    let d;
    if (edge.channel !== undefined) {
      const horizontal = fromSide === "left" || fromSide === "right";
      d = horizontal
        ? `M${a.x},${a.y} C${edge.channel},${a.y} ${edge.channel},${b.y} ${b.x},${b.y}`
        : `M${a.x},${a.y} C${a.x},${edge.channel} ${b.x},${edge.channel} ${b.x},${b.y}`;
    } else if (fromSide === "left" || fromSide === "right") {
      const dx = b.x - a.x;
      d = `M${a.x},${a.y} C${a.x + dx * 0.45},${a.y} ${b.x - dx * 0.45},${b.y} ${b.x},${b.y}`;
    } else {
      const dy = b.y - a.y;
      d = `M${a.x},${a.y} C${a.x},${a.y + dy * 0.45} ${b.x},${b.y - dy * 0.45} ${b.x},${b.y}`;
    }
    return { d, a, b };
  };

  const drawPosterHeader = root => {
    if (!poster || !data.poster) return;
    const group = svgElement("g", { id: "poster-header" });
    group.appendChild(svgElement("rect", { x: 0, y: 0, width: 1470, height: 110, fill: "#F0DEC2" }));
    group.appendChild(svgElement("rect", { x: 0, y: 109, width: 1470, height: 1, fill: "#D5BA91" }));
    const squares = [[6, 25], [17, 25], [28, 8], [28, 25], [28, 42], [39, 25], [39, 42], [50, 25]];
    squares.forEach(([x, y], index) => group.appendChild(svgElement("rect", {
      x: x + 35, y: y + 17, width: 9, height: 9, rx: 2, fill: index % 3 === 0 ? "#BB7738" : "#543A29"
    })));
    group.appendChild(svgElement("text", { x: 105, y: 62, fill: "#543A29", "font-family": "Outfit, sans-serif", "font-size": 38, "font-weight": 700 }, "tako"));
    group.appendChild(svgElement("text", { x: 232, y: 46, fill: "#543A29", "font-family": "Outfit, PingFang SC, sans-serif", "font-size": 28, "font-weight": 650 }, data.poster.title));
    group.appendChild(svgElement("text", { x: 232, y: 76, fill: "#765C45", "font-family": "Outfit, PingFang SC, sans-serif", "font-size": 13 }, data.poster.subtitle));
    group.appendChild(svgElement("text", { x: 1030, y: 40, fill: "#765C45", "font-family": "JetBrains Mono, monospace", "font-size": 11 }, data.poster.meta));
    group.appendChild(svgElement("text", { x: 1030, y: 65, fill: "#955A2C", "font-family": "JetBrains Mono, monospace", "font-size": 11 }, data.poster.baseline));
    root.appendChild(group);
  };

  const drawGraph = () => {
    svg.setAttribute("viewBox", viewBox.join(" "));
    svg.setAttribute("aria-label", data.diagramLabel || data.title);
    const defs = svgElement("defs");
    const marker = svgElement("marker", {
      id: "arrowhead", viewBox: "0 0 10 10", refX: 9, refY: 5,
      markerWidth: 6, markerHeight: 6, orient: "auto-start-reverse"
    });
    marker.appendChild(svgElement("path", { d: "M0,1 L9,5 L0,9 z", fill: "context-stroke" }));
    defs.appendChild(marker);
    svg.appendChild(defs);
    drawPosterHeader(svg);
    const root = svgElement("g", { id: "graph-root" });
    const bandRoot = svgElement("g", { id: "band-root" });
    const edgeRoot = svgElement("g", { id: "edge-root" });
    const labelRoot = svgElement("g", { id: "label-root" });
    const nodeRoot = svgElement("g", { id: "node-root" });
    root.append(bandRoot, edgeRoot, labelRoot, nodeRoot);
    svg.appendChild(root);
    svg.appendChild(svgElement("circle", { id: "pulse-dot", class: "pulse-dot", r: 6, cx: 0, cy: 0 }));

    (data.bands || []).forEach((band, index) => {
      const group = svgElement("g", { class: "diagram-band-group" });
      group.appendChild(svgElement("rect", {
        class: `diagram-band${index % 2 ? " diagram-band-alt" : ""}`,
        x: band.x, y: band.y, width: band.w, height: band.h, rx: 14
      }));
      group.appendChild(svgElement("text", { class: "band-title", x: band.x + 17, y: band.y + 26 }, band.title));
      if (band.subtitle) group.appendChild(svgElement("text", { class: "band-sub", x: band.x + 17, y: band.y + 44 }, band.subtitle));
      bandRoot.appendChild(group);
    });

    data.edges.forEach(edge => {
      const geo = geometry(edge);
      const group = svgElement("g", {
        class: `edge-group edge-${edge.kind || "support"}`,
        tabindex: "0",
        role: "button",
        "aria-label": `${edge.label}，查看详情`,
        "data-edge": edge.id
      });
      const path = svgElement("path", {
        class: "edge-line",
        d: geo.d,
        "marker-end": "url(#arrowhead)",
        "marker-start": edge.bidirectional ? "url(#arrowhead)" : ""
      });
      const hit = svgElement("path", { class: "edge-hit", d: geo.d });
      group.append(path, hit);
      group.addEventListener("click", () => selectEdge(edge.id));
      group.addEventListener("keydown", event => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          selectEdge(edge.id);
        }
      });
      edgeRoot.appendChild(group);
      const label = svgElement("text", {
        class: "edge-label",
        x: edge.labelAt ? edge.labelAt[0] : (geo.a.x + geo.b.x) / 2,
        y: edge.labelAt ? edge.labelAt[1] : (geo.a.y + geo.b.y) / 2 - 5,
        "text-anchor": "middle"
      }, edge.short || edge.label);
      if (poster && !(data.steps || []).some(step => step.edge === edge.id)) label.style.display = "none";
      labelRoot.appendChild(label);
      edges.set(edge.id, { data: edge, group, path, label, geo });
    });

    data.nodes.forEach(node => {
      const group = svgElement("g", {
        class: "diagram-node",
        tabindex: "0",
        role: "button",
        "aria-label": `${node.title}，${node.subtitle || "查看详情"}`,
        "data-node": node.id
      });
      group.appendChild(svgElement("rect", { class: "node-box", x: node.x, y: node.y, width: node.w, height: node.h, rx: 10 }));
      group.appendChild(svgElement("rect", { class: "node-bar", x: node.x, y: node.y + 10, width: 3, height: Math.max(10, node.h - 20), rx: 1.5 }));
      group.appendChild(svgElement("text", { class: "node-title", x: node.x + 15, y: node.y + 24 }, fitNodeText(node.title, node, 20)));
      if (node.subtitle) group.appendChild(svgElement("text", { class: "node-subtitle", x: node.x + 15, y: node.y + 43 }, fitNodeText(node.subtitle, node, 15)));
      (node.lines || []).forEach((line, index) => group.appendChild(svgElement("text", {
        class: "node-subtitle", x: node.x + 15, y: node.y + 64 + index * 16
      }, fitNodeText(line, node, 15))));
      group.addEventListener("click", () => selectNode(node.id));
      group.addEventListener("keydown", event => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          selectNode(node.id);
        }
      });
      nodeRoot.appendChild(group);
      nodes.set(node.id, { data: node, group });
    });
    updateTransform();
  };

  const renderFilters = () => {
    if (!filters) return;
    const entries = Object.entries(data.flows || {});
    if (!entries.length) {
      filters.closest(".control-section").hidden = true;
      return;
    }
    entries.forEach(([key, label]) => {
      const item = document.createElement("label");
      item.className = "filter-item";
      item.innerHTML = `<input type="checkbox" checked value="${escapeHTML(key)}"><span class="flow-swatch ${escapeHTML(key)}" aria-hidden="true"></span><span>${escapeHTML(label)}</span>`;
      item.querySelector("input").addEventListener("change", event => {
        if (event.target.checked) state.flows.add(key);
        else state.flows.delete(key);
        applyVisibility();
      });
      filters.appendChild(item);
    });
  };

  const applyVisibility = () => {
    const query = state.query.trim().toLowerCase();
    const hits = new Set();
    nodes.forEach((record, id) => {
      const node = record.data;
      const haystack = [node.title, node.subtitle, node.summary, node.input, node.output, (node.code || []).join(" "), (node.docs || []).map(item => item.path || item).join(" ")].join(" ").toLowerCase();
      const visible = !query || haystack.includes(query);
      if (visible) hits.add(id);
      record.group.classList.toggle("dimmed", !visible);
    });
    edges.forEach(record => {
      const flowVisible = state.flows.has(record.data.kind || "support");
      const searchVisible = !query || hits.has(record.data.from) || hits.has(record.data.to);
      record.group.classList.toggle("dimmed", !(flowVisible && searchVisible));
      record.label.classList.toggle("dimmed", !(flowVisible && searchVisible));
    });
    if (searchStatus) searchStatus.textContent = query ? `${hits.size} 个匹配节点` : "";
  };

  const clearHighlights = () => {
    nodes.forEach(record => record.group.classList.remove("selected"));
    edges.forEach(record => record.group.classList.remove("selected"));
  };
  const selectNode = id => {
    stopPlaying();
    state.selected = { type: "node", id };
    clearHighlights();
    const record = nodes.get(id);
    if (!record) return;
    record.group.classList.add("selected");
    edges.forEach(edge => {
      if (edge.data.from === id || edge.data.to === id) edge.group.classList.add("selected");
    });
    const node = record.data;
    const upstream = data.edges.filter(edge => edge.to === id).map(edge => nodes.get(edge.from)?.data.title).filter(Boolean);
    const downstream = data.edges.filter(edge => edge.from === id).map(edge => nodes.get(edge.to)?.data.title).filter(Boolean);
    detail.innerHTML = `
      <p class="detail-kicker">节点 · ${escapeHTML(node.band || "系统模块")}</p>
      <h2>${escapeHTML(node.title)}</h2>
      <p class="detail-subtitle">${escapeHTML(node.subtitle || "")}</p>
      <p>${inline(node.summary)}</p>
      ${node.need ? `<h3>没有它会怎样</h3><p>${inline(node.need)}</p>` : ""}
      ${node.input ? `<h3>输入从哪来</h3><p>${inline(node.input)}</p>` : ""}
      ${node.output ? `<h3>输出到哪去</h3><p>${inline(node.output)}</p>` : ""}
      ${node.facts?.length ? `<h3>关键事实</h3><dl class="detail-facts">${node.facts.map(fact => `<dt>${escapeHTML(fact[0])}</dt><dd>${inline(fact[1])}</dd>`).join("")}</dl>` : ""}
      <h3>上游</h3><p>${upstream.length ? upstream.map(escapeHTML).join("、") : "入口节点"}</p>
      <h3>下游</h3><p>${downstream.length ? downstream.map(escapeHTML).join("、") : "终点或旁路节点"}</p>
      ${node.code?.length ? `<h3>代码落点</h3><div class="detail-links">${localLinks(node.code, CODE_BASE)}</div>` : ""}
      ${node.docs?.length ? `<h3>现行文档</h3><div class="detail-links">${localLinks(node.docs, "../")}</div>` : ""}`;
  };
  const selectEdge = (id, keepPlaying = false) => {
    if (!keepPlaying) stopPlaying();
    state.selected = { type: "edge", id };
    clearHighlights();
    const record = edges.get(id);
    if (!record) return;
    record.group.classList.add("selected");
    nodes.get(record.data.from)?.group.classList.add("selected");
    nodes.get(record.data.to)?.group.classList.add("selected");
    const related = (data.steps || []).map((step, index) => ({ ...step, index })).filter(step => step.edge === id);
    detail.innerHTML = `
      <p class="detail-kicker">数据流 · ${escapeHTML(data.flows?.[record.data.kind || "support"] || "连接")}</p>
      <h2>${escapeHTML(nodes.get(record.data.from).data.title)} → ${escapeHTML(nodes.get(record.data.to).data.title)}</h2>
      <p class="detail-subtitle">${escapeHTML(record.data.short || record.data.label)}</p>
      <p>${inline(record.data.description || record.data.label)}</p>
      ${related.length ? `<h3>在链路里的位置</h3>${related.map(step => `<p><strong>${escapeHTML(step.title)}</strong>：${inline(step.text)}</p>`).join("")}` : ""}
      <h3>起点与终点</h3><dl class="detail-facts"><dt>起点</dt><dd>${escapeHTML(nodes.get(record.data.from).data.title)}</dd><dt>终点</dt><dd>${escapeHTML(nodes.get(record.data.to).data.title)}</dd><dt>方向</dt><dd>${record.data.bidirectional ? "双向：调用与结果" : "单向"}</dd></dl>`;
  };
  const defaultDetail = () => {
    const guide = data.guide || {};
    detail.innerHTML = `
      <p class="detail-kicker">导读</p>
      <h2>${escapeHTML(guide.title || "怎么读这张图")}</h2>
      ${(guide.paragraphs || []).map(text => `<p>${inline(text)}</p>`).join("")}
      ${guide.bullets?.length ? `<h3>先记住这些</h3><ul>${guide.bullets.map(item => `<li>${inline(item)}</li>`).join("")}</ul>` : ""}`;
  };

  const pulseEdge = edge => {
    stopPulse();
    if (reducedMotion.matches || poster) return;
    const pulse = document.getElementById("pulse-dot");
    const length = edge.path.getTotalLength();
    const start = performance.now();
    const frame = now => {
      const progress = Math.min(1, (now - start) / 850);
      const point = edge.path.getPointAtLength(length * progress);
      pulse.setAttribute("cx", point.x);
      pulse.setAttribute("cy", point.y);
      pulse.style.opacity = progress < 1 ? "1" : "0";
      if (progress < 1) state.raf = requestAnimationFrame(frame);
    };
    state.raf = requestAnimationFrame(frame);
  };
  const renderStep = index => {
    const steps = data.steps || [];
    if (!steps.length) return;
    state.step = Math.max(0, Math.min(steps.length - 1, index));
    const step = steps[state.step];
    selectEdge(step.edge, true);
    pulseEdge(edges.get(step.edge));
    detail.innerHTML += `
      <h3>第 ${state.step + 1} / ${steps.length} 步 · ${escapeHTML(step.title)}</h3>
      <p>${inline(step.text)}</p>
      <h3>全部步骤</h3>
      <ol class="step-list">${steps.map((item, itemIndex) => `<li class="${itemIndex === state.step ? "current" : ""}"><button type="button" data-step="${itemIndex}">${escapeHTML(item.title)}</button></li>`).join("")}</ol>`;
    detail.querySelectorAll("[data-step]").forEach(button => button.addEventListener("click", () => {
      stopPlaying();
      renderStep(Number(button.dataset.step));
    }));
    if (previousButton) previousButton.disabled = state.step === 0;
    if (nextButton) nextButton.disabled = state.step === steps.length - 1;
  };
  const scheduleNext = () => {
    if (!state.playing) return;
    state.timer = setTimeout(() => {
      if (state.step >= data.steps.length - 1) {
        stopPlaying();
        return;
      }
      renderStep(state.step + 1);
      scheduleNext();
    }, 3600);
  };

  const renderMobile = () => {
    const steps = data.steps?.length ? `
      <section class="control-section">
        <p class="detail-kicker">按顺序阅读</p>
        <ol class="numbered-flow">${data.steps.map((step, index) => `<li><div><h3>${index + 1}. ${escapeHTML(step.title)}</h3><p>${inline(step.text)}</p></div></li>`).join("")}</ol>
      </section>` : "";
    mobileView.innerHTML = `${steps}<ul class="mobile-list">${data.nodes.map(node => `
      <li class="mobile-item">
        <p class="mono">${escapeHTML(node.band || "系统模块")}</p>
        <h2>${escapeHTML(node.title)}</h2>
        <p class="mono">${escapeHTML(node.subtitle || "")}</p>
        <p>${inline(node.summary)}</p>
        ${node.need ? `<p><strong>没有它：</strong>${inline(node.need)}</p>` : ""}
        ${node.input ? `<p><strong>输入：</strong>${inline(node.input)}</p>` : ""}
        ${node.output ? `<p><strong>输出：</strong>${inline(node.output)}</p>` : ""}
        ${node.code?.length ? `<div class="row-links">${localLinks(node.code, CODE_BASE)}</div>` : ""}
        ${node.docs?.length ? `<div class="row-links">${localLinks(node.docs, "../")}</div>` : ""}
      </li>`).join("")}</ul>`;
  };

  const renderSections = () => {
    content.innerHTML = (data.sections || []).map(section => {
      let bodyHTML = "";
      if (section.type === "table") {
        bodyHTML = `<div class="table-wrap" tabindex="0" aria-label="${escapeHTML(section.title)}，横向表格"><table class="doc-table"><thead><tr>${section.columns.map(column => `<th>${escapeHTML(column)}</th>`).join("")}</tr></thead><tbody>${section.rows.map(row => `<tr>${row.map(cell => `<td>${inline(cell)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
      } else if (section.type === "flow") {
        bodyHTML = `<ol class="numbered-flow">${section.items.map(item => `<li><div><h3>${escapeHTML(item.title)}</h3><p>${inline(item.text)}</p>${item.meta ? `<p class="row-meta">${inline(item.meta)}</p>` : ""}</div></li>`).join("")}</ol>`;
      } else if (section.type === "tree") {
        bodyHTML = `<pre class="code-tree" tabindex="0">${escapeHTML(section.text)}</pre>`;
      } else {
        bodyHTML = `<div class="section-rows">${(section.items || []).map(item => `
          <div class="section-row">
            <div><h3>${escapeHTML(item.title)}</h3>${item.meta ? `<p class="row-meta">${inline(item.meta)}</p>` : ""}</div>
            <div><p>${inline(item.text)}</p>${item.text2 ? `<p>${inline(item.text2)}</p>` : ""}${item.links?.length ? `<div class="row-links">${item.links.map(link => `<a href="${escapeHTML(link.href)}">${escapeHTML(link.label)}</a>`).join("")}</div>` : ""}</div>
          </div>`).join("")}</div>`;
      }
      return `<section class="content-section"><div class="section-heading"><h2>${escapeHTML(section.title)}</h2><p>${inline(section.intro || "")}</p></div>${bodyHTML}</section>`;
    }).join("");
  };

  const reset = () => {
    stopPlaying();
    state.selected = null;
    state.step = -1;
    state.scale = 1;
    state.tx = 0;
    state.ty = 0;
    state.query = "";
    state.flows = new Set(Object.keys(data.flows || {}));
    if (search) search.value = "";
    filters?.querySelectorAll("input").forEach(input => { input.checked = true; });
    clearHighlights();
    if (previousButton) previousButton.disabled = true;
    if (nextButton) nextButton.disabled = false;
    updateTransform();
    applyVisibility();
    defaultDetail();
  };

  document.querySelectorAll(".page-nav a").forEach(link => {
    if (link.dataset.page === body.dataset.page) link.setAttribute("aria-current", "page");
  });
  document.querySelectorAll("[data-view-choice]").forEach(button => {
    button.addEventListener("click", () => {
      body.dataset.view = button.dataset.viewChoice;
      document.querySelectorAll("[data-view-choice]").forEach(item => item.setAttribute("aria-pressed", String(item === button)));
      if (button.dataset.viewChoice === "list") stopPlaying();
    });
  });
  search?.addEventListener("input", event => {
    state.query = event.target.value;
    applyVisibility();
    if (state.query && ![...nodes.values()].some(record => !record.group.classList.contains("dimmed"))) {
      detail.innerHTML = `<div class="empty-state">没有匹配节点。换一个模块名、工具名或代码路径试试。</div>`;
    } else if (!state.selected) defaultDetail();
  });
  document.getElementById("reset")?.addEventListener("click", reset);
  document.getElementById("zoom-in")?.addEventListener("click", () => zoomAt(viewBox[2] / 2, viewBox[3] / 2, 1.2));
  document.getElementById("zoom-out")?.addEventListener("click", () => zoomAt(viewBox[2] / 2, viewBox[3] / 2, 1 / 1.2));
  document.getElementById("zoom-fit")?.addEventListener("click", () => {
    state.scale = 1;
    state.tx = 0;
    state.ty = 0;
    updateTransform();
  });
  playButton?.addEventListener("click", () => {
    if (state.playing) {
      stopPlaying();
      return;
    }
    state.playing = true;
    playButton.textContent = "暂停播放";
    playButton.setAttribute("aria-pressed", "true");
    renderStep(state.step < 0 || state.step >= data.steps.length - 1 ? 0 : state.step);
    scheduleNext();
  });
  previousButton?.addEventListener("click", () => {
    stopPlaying();
    renderStep(state.step < 0 ? 0 : state.step - 1);
  });
  nextButton?.addEventListener("click", () => {
    stopPlaying();
    renderStep(state.step + 1);
  });
  svg.addEventListener("wheel", event => {
    if (poster) return;
    event.preventDefault();
    const point = svgPoint(event);
    zoomAt(point.x, point.y, event.deltaY < 0 ? 1.12 : 0.89);
  }, { passive: false });
  let drag = null;
  svg.addEventListener("pointerdown", event => {
    if (poster || event.pointerType !== "mouse" || event.button !== 0 || event.target.closest(".diagram-node,.edge-group")) return;
    drag = { x: event.clientX, y: event.clientY, tx: state.tx, ty: state.ty };
    svg.setPointerCapture(event.pointerId);
    svg.classList.add("dragging");
  });
  svg.addEventListener("pointermove", event => {
    if (!drag) return;
    state.tx = drag.tx + (event.clientX - drag.x) * viewBox[2] / svg.clientWidth;
    state.ty = drag.ty + (event.clientY - drag.y) * viewBox[3] / svg.clientHeight;
    updateTransform();
  });
  const endDrag = () => { drag = null; svg.classList.remove("dragging"); };
  svg.addEventListener("pointerup", endDrag);
  svg.addEventListener("pointercancel", endDrag);
  reducedMotion.addEventListener("change", () => { if (reducedMotion.matches) stopPlaying(); });
  window.addEventListener("pagehide", stopPlaying);

  if (poster) {
    body.classList.add("poster");
    document.title = `${data.poster?.title || data.title} · 海报`;
  }
  if (!data.steps?.length) {
    document.querySelectorAll(".playback-control").forEach(element => { element.hidden = true; });
  }
  drawGraph();
  renderFilters();
  renderMobile();
  renderSections();
  reset();
  body.dataset.ready = "true";
})();
