"use strict";
const appRoot = new URL("../", document.currentScript.src);
const appURL = (path) => new URL(path, appRoot).pathname;
document.querySelector(".brand").href = appRoot.href;
const $ = (selector, root = document) => root.querySelector(selector);
const node = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
};
const short = (sha) => sha.slice(0, 10);
const date = (value) =>
  new Date(value).toLocaleString(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
const bytes = (value) => {
  const n = Math.abs(value);
  let unit = 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  while (unit < 4 && n / 1024 ** unit >= 1024) unit++;
  return (
    (n / 1024 ** unit).toLocaleString(undefined, {
      maximumFractionDigits: unit ? 1 : 0,
    }) +
    " " +
    units[unit]
  );
};
const delta = (value) =>
  (value > 0 ? "+" : value < 0 ? "−" : "") + bytes(value);
let repositories = [],
  route = null,
  attributes = [],
  selected = "",
  requestID = 0,
  report = null;
let controller = null;
let sortKey = "name",
  sortDirection = 1;
function error(message) {
  $("#notice").textContent = message;
  $("#notice").hidden = !message;
}
async function get(url, signal) {
  const response = await fetch(url, { signal });
  const data = await response.json();
  if (!response.ok && response.status !== 202)
    throw new Error(data.error || "Request failed.");
  return data;
}
function readRoute() {
  const parts = location.pathname.slice(appRoot.pathname.length).split("/").filter(Boolean);
  return parts.length === 4
    ? {
        owner: decodeURIComponent(parts[0]),
        repo: decodeURIComponent(parts[1]),
        old: parts[2],
        new: parts[3],
      }
    : null;
}
function baseAPI() {
  return appURL(`api/comparisons/${encodeURIComponent(route.owner)}/${encodeURIComponent(route.repo)}/${route.old}/${route.new}`);
}
function comparisonURL(owner, repo, old, next) {
  return appURL(`${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${old}/${next}`);
}
function selectedRepo() {
  return repositories.find((r) => r.owner + "/" + r.name === $("#repo").value);
}
function commitMeta(c) {
  return [
    short(c.sha),
    c.date && !c.date.startsWith("0001") ? date(c.date) : "",
    ...(c.branches || []),
  ]
    .filter(Boolean)
    .join(" · ");
}
function updateCommitChoices() {
  const commits = selectedRepo()?.commits || [];
  const old = commits.find((c) => c.sha === $("#old").value),
    next = commits.find((c) => c.sha === $("#new").value);
  for (const option of $("#old").options) {
    const c = commits.find((c) => c.sha === option.value);
    option.disabled = !!next && (!c || c.order <= next.order);
  }
  for (const option of $("#new").options) {
    const c = commits.find((c) => c.sha === option.value);
    option.disabled = !!old && (!c || c.order >= old.order);
  }
  for (const [side, commit] of [["old", old], ["new", next]]) {
    const meta = $("#" + side + "-meta");
    meta.replaceChildren();
    if (!commit) continue;
    const repo = selectedRepo();
    const link = node("a", "", short(commit.sha));
    link.href = `https://github.com/${encodeURIComponent(repo.owner)}/${encodeURIComponent(repo.name)}/commit/${commit.sha}`;
    link.target = "_blank";
    link.rel = "noreferrer";
    meta.append(link, commitMeta(commit).slice(short(commit.sha).length));
  }
  $(".compare-button").disabled =
    !old ||
    !next ||
    old.order <= next.order ||
    !!selectedRepo()?.metadata_error;
}
function setCommitOptions(fill) {
  const commits = selectedRepo()?.commits || [];
  for (const side of ["old", "new"]) {
    const select = $("#" + side);
    select.replaceChildren();
    const placeholder = node("option", "", "Choose a commit");
    placeholder.value = "";
    select.append(placeholder);
    for (const c of commits) {
      const option = node("option", "", c.message || "Metadata unavailable");
      option.value = c.sha;
      option.title = commitMeta(c);
      select.append(option);
    }
  }
  if (fill) {
    $("#old").value = commits[1]?.sha || "";
    $("#new").value = commits[0]?.sha || "";
  }
  updateCommitChoices();
}
function renderHome() {
  const list = $("#repositories");
  list.replaceChildren();
  if (!repositories.length) {
    list.append(node("div", "empty", "No configuration snapshots yet."));
    return;
  }
  for (const repo of repositories) {
    const section = node("section", "repository");
    section.append(node("h3", "", repo.owner + "/" + repo.name));
    if (repo.metadata_error)
      section.append(node("p", "muted", repo.metadata_error));
    const ul = node("ul", "commit-list");
    repo.recent_commits.forEach((commit, index) => {
      const li = node("li", "commit-row"),
        identity = node("div");
      identity.append(
        node(
          "span",
          "commit-message",
          commit.message || "Metadata unavailable",
        ),
        node("span", "commit-secondary", commitMeta(commit)),
      );
      li.append(identity);
      const button = node("button", "text-button", "Compare with previous");
      button.disabled =
        index === repo.commits.length - 1 || !!repo.metadata_error;
      button.addEventListener("click", () => {
        const older = repo.commits[index + 1];
        $("#repo").value = repo.owner + "/" + repo.name;
        setCommitOptions(false);
        $("#old").value = older.sha;
        $("#new").value = commit.sha;
        updateCommitChoices();
        $("#compare-form").requestSubmit();
      });
      li.append(button);
      ul.append(li);
    });
    section.append(ul);
    list.append(section);
  }
}
async function initialize() {
  try {
    repositories = await get(appURL("api/repos"));
    const select = $("#repo");
    select.replaceChildren();
    for (const r of repositories) {
      const option = node("option", "", r.owner + "/" + r.name);
      option.value = r.owner + "/" + r.name;
      select.append(option);
    }
    route = readRoute();
    if (route) {
      const value = route.owner + "/" + route.repo;
      if (!repositories.some((r) => r.owner + "/" + r.name === value)) {
        const option = node("option", "", value);
        option.value = value;
        select.append(option);
      }
      select.value = value;
      setCommitOptions(false);
      $("#old").value = route.old;
      $("#new").value = route.new;
      updateCommitChoices();
      await loadComparison();
    } else {
      setCommitOptions(true);
      renderHome();
    }
    if (!repositories.length && !route) {
      select.append(node("option", "", "No repositories yet"));
    }
  } catch (e) {
    error(e.message);
  }
}
function selectionChanged() {
  updateCommitChoices();
  if (route && !$(".compare-button").disabled) {
    $("#compare-form").requestSubmit();
  }
}
$("#repo").addEventListener("change", () => {
  setCommitOptions(true);
  selectionChanged();
});
$("#old").addEventListener("change", selectionChanged);
$("#new").addEventListener("change", selectionChanged);
$("#compare-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const [owner, repo] = $("#repo").value.split("/");
  if (!owner || !repo || $(".compare-button").disabled) return;
  const url = comparisonURL(
    owner,
    repo,
    $("#old").value.trim(),
    $("#new").value.trim(),
  );
  history.pushState({}, "", url);
  route = readRoute();
  error("");
  loadComparison().catch((e) => error(e.message));
});
window.addEventListener("popstate", () => location.reload());
async function loadComparison() {
  $(".compare-button").hidden = true;
  $("#compare-form").classList.add("in-diff");
  controller?.abort();
  const indexToken = ++requestID;
  $("#home").hidden = true;
  $("#comparison").hidden = false;
  document.title = `${short(route.old)} → ${short(route.new)} · nix diffs`;
  if (!$(".table-wrap", $("#detail")))
    showState("Loading comparison", "Counting package changes…");
  const data = await get(baseAPI());
  if (indexToken !== requestID) return;
  if (data.status === "not_ready") {
    attributes = [];
    renderHosts();
    showState(
      "Build not ready",
      "Snapshots for these commits have not arrived yet.",
    );
    return;
  }
  attributes = data.attributes;
  const query = new URLSearchParams(location.search);
  selected = selected || query.get("attr") || "";
  if (!attributes.some((a) => a.id === selected)) selected = attributes[0]?.id || "";
  renderHosts();
  if (selected) {
    await openHost(selected);
  } else {
    const url = new URL(location.href);
    url.searchParams.delete("attr");
    history.replaceState({}, "", url);
    showState("No package changes", "No configurations with package changes to show.");
  }
}

function renderHosts() {
  const visible = attributes;
  const list = $("#hosts");
  list.replaceChildren();
  for (const attr of visible) {
    const button = node(
      "button",
      "host" + (attr.id === selected ? " active" : ""),
    );
    button.type = "button";
    button.setAttribute("aria-pressed", String(attr.id === selected));
    const heading = node("span", "host-heading");
    const count = node("span", "host-change-count", attr.changes == null ? (attr.countFailed || !attr.old || !attr.new ? "—" : "…") : attr.changes.toLocaleString());
    count.title = attr.changes == null ? "Package changes pending" : `${attr.changes} package changes`;
    if (attr.id === "all") {
      heading.classList.add("all-hosts-heading");
    } else {
      const darwin = attr.platform?.endsWith("-darwin");
      const icon = node("img", "platform-icon");
      icon.src = appURL(`static/${darwin ? "apple" : "tux"}.svg`);
      icon.alt = darwin ? "Darwin" : "Linux";
      heading.append(icon);
    }
    heading.append(node("span", "host-name", attr.name), count);
    button.append(heading);
    if (!attr.old || !attr.new)
      button.append(
        node("span", "host-meta host-pending", "Awaiting snapshot"),
      );
    button.addEventListener("click", () => openHost(attr.id));
    list.append(button);
  }
  if (!visible.length)
    list.append(node("p", "muted", "No matching configurations."));
}

function showState(title, message, retry) {
  const box = node("div", "empty");
  box.append(node("h2", "", title), node("p", "", message));
  if (retry) {
    const button = node("button", "secondary", "Try again");
    button.addEventListener("click", () => openHost(selected));
    box.append(button);
  }
  $("#detail").replaceChildren(box);
}
async function openHost(id) {
  selected = id;
  renderHosts();
  const url = new URL(location.href);
  url.searchParams.set("attr", id);
  history.replaceState({}, "", url);
  controller?.abort();
  controller = new AbortController();
  const token = ++requestID;
  const detail = $("#detail");
  detail.setAttribute("aria-busy", "true");
  const loadingTimer = setTimeout(() => {
    if (token === requestID && !$(".table-wrap", detail))
      showState("Loading comparison", "Comparing snapshots…");
  }, 200);
  try {
    const data = await get(
      baseAPI() + "?attr=" + encodeURIComponent(id),
      controller.signal,
    );
    if (token !== requestID) return;
    if (data.status === "not_ready") {
      const attr = attributes.find((a) => a.id === id);
      const missing =
        attr && !attr.old && !attr.new
          ? "Both snapshots"
          : attr && !attr.old
            ? "The “from” snapshot"
            : "The “to” snapshot";
      showState(
        "Build not ready",
        `${missing} for this output has not arrived yet. Refresh snapshots after the build completes.`,
        true,
      );
      return;
    }
    report = data.report;
    renderReport(data);
  } catch (e) {
    if (e.name !== "AbortError" && token === requestID)
      showState("Could not compare snapshots", e.message, true);
  } finally {
    clearTimeout(loadingTimer);
    if (token === requestID) detail.removeAttribute("aria-busy");
  }
}
function renderReport(data) {
  const root = $("#detail");
  root.replaceChildren($("#report-template").content.cloneNode(true));
  const attr = attributes.find((a) => a.id === selected) || {
    name: selected,
    platform: "",
    kind: "Output",
  };
  $(".host-title", root).textContent = attr.name;
  if (attr.pending) {
    $(".detail-heading", root).append(node("span", "muted", `${attr.pending} hosts awaiting snapshots`));
  }
  const change = report.size_new - report.size_old;
  $(".closure-delta", root).textContent = delta(change);
  $(".closure-delta", root).classList.add(
    change > 0 ? "size-grow" : change < 0 ? "size-shrink" : "neutral",
  );

  for (const id of ["package-search", "selected-only"])
    $("#" + id).addEventListener(
      id === "package-search" ? "input" : "change",
      () => {

        renderRows();
      },
    );
  sortKey = "name";
  sortDirection = 1;
  for (const button of root.querySelectorAll(".table-sort")) {
    button.addEventListener("click", () => {
      if (sortKey === button.dataset.sort) {
        sortDirection *= -1;
      } else {
        sortKey = button.dataset.sort;
        sortDirection = sortKey === "size" ? -1 : 1;
      }

      renderRows();
    });
  }
  renderRows();
}
function versions(diff) {
  const before = [],
    after = [];
  const label = (v) =>
    `${v.name || "(unversioned)"}${v.amount > 1 ? " ×" + v.amount : ""}`;
  for (const version of diff.versions) {
    if (version.kind === "changed") {
      before.push(label(version.old));
      after.push(label(version.new));
    } else if (version.kind === "added") {
      before.push("—");
      after.push(label(version.version));
    } else if (version.kind === "removed") {
      before.push(label(version.version));
      after.push("—");
    } else if (version.kind === "amount_changed") {
      before.push(label({ ...version.version, amount: version.old_amount }));
      after.push(label({ ...version.version, amount: version.new_amount }));
    }
  }
  return [before.length ? before : ["—"], after.length ? after : ["—"]];
}
function renderRows() {
  const q = $("#package-search").value.toLowerCase(),
    only = $("#selected-only").checked;
  let rows = report.diffs.filter(
    (d) =>
      (!only || d.selection !== "Unselected") &&
      (d.name + " " + versions(d).flat().join(" ")).toLowerCase().includes(q),
  );
  rows.sort((a, b) => {
    let value;
    if (sortKey === "size") {
      value = a.size_delta - b.size_delta;
    } else {
      value = a.name.localeCompare(b.name);
    }
    return value * sortDirection || a.name.localeCompare(b.name);
  });
  for (const button of document.querySelectorAll(".table-sort")) {
    const active = button.dataset.sort === sortKey;
    button
      .closest("th")
      .setAttribute(
        "aria-sort",
        active ? (sortDirection === 1 ? "ascending" : "descending") : "none",
      );
    button.querySelector("span").textContent = active
      ? sortDirection === 1
        ? "↑"
        : "↓"
      : "↕";
  }
  const tbody = $("tbody");
  tbody.replaceChildren();
  for (const diff of rows) {
    const tr = node("tr");
    const name = node("td");
    const heading = node("span", "package-heading");
    heading.append(
      node("span", "package-name", diff.name),
      node("span", "tag tag-" + diff.status.toLowerCase(), diff.status),
    );
    name.append(heading);
    if (
      diff.selection === "NewlySelected" ||
      diff.selection === "NewlyUnselected"
    )
      name.append(
        node(
          "span",
          "version-detail",
          diff.selection === "NewlySelected"
            ? "Now explicitly selected"
            : "No longer explicitly selected",
        ),
      );
    tr.append(name);
    for (const list of versions(diff)) {
      const td = node("td");
      for (const version of list) td.append(node("span", "version", version));
      tr.append(td);
    }
    const size = node(
      "td",
      "number " +
        (diff.size_delta > 0
          ? "size-grow"
          : diff.size_delta < 0
            ? "size-shrink"
            : ""),
      delta(diff.size_delta),
    );
    size.title = `${bytes(diff.size_old)} → ${bytes(diff.size_new)}`;
    tr.append(size);
    tbody.append(tr);
  }
  $(".table-wrap").hidden = !rows.length;
  $(".table-empty").hidden = !!rows.length;
  $(".table-empty").textContent = report.diffs.length
    ? "No packages match these filters."
    : "No package changes. Closure totals above may still differ.";

}
initialize();
