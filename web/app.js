const $ = (sel) => document.querySelector(sel);
const yourBoardEl = $("#yourBoard");
const oppBoardEl = $("#oppBoard");
const statusEl = $("#status");
const startBtn = $("#startBtn");
const opponentUrlInput = $("#opponentUrl");

let incomingOnMyBoard = {};
let lastIncomingN = 0;

// local state
let yourBoard = null;
let opponent = null;
let shotState = {};

function setStatus(text, ok = true) {
  statusEl.textContent = text;
  statusEl.style.color = ok ? "#14532d" : "#7f1d1d";
}
function gridKey(r,c) { return `${r},${c}`; }

async function requestJSON(url, method = "GET", body) {
  const res = await fetch(url, {
    method,
    headers: {"content-type":"application/json"},
    body: body ? JSON.stringify(body) : undefined
  });
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try { const d = await res.json(); if (d && d.error) msg += `: ${d.error}` } catch {}
    throw new Error(msg);
  }
  return await res.json();
}
const getJSON = (url) => requestJSON(url, "GET");
const postJSON = (url, obj) => requestJSON(url, "POST", obj);
const putJSON  = (url, obj) => requestJSON(url, "PUT", obj);

async function readStatus() {
  try {
    return await getJSON('/v1/status');
  } catch (e) {
    return null;
  }
}

async function refreshTurn() {
  const s = await readStatus();
  if (!s || !s.turn) {
    oppBoardEl.style.pointerEvents = 'none';
    oppBoardEl.style.opacity = '0.5';
    return;
  }
  const t = s.turn;
  const canClick = t.decided === true && t.ready === true && t.myTurn === 'me';
  statusEl.textContent = canClick ? "Your turn" :
                         (t && t.decided ? "Opponent’s turn" : "Deciding turns…");
  oppBoardEl.style.pointerEvents = canClick ? 'auto' : 'none';
  oppBoardEl.style.opacity = canClick ? '1' : '0.5';
}

async function refreshGameState() {
  const s = await readStatus();
  if (!s || !s.game) return false;
  const g = s.game;
  if (g.over) {
    oppBoardEl.style.pointerEvents = 'none';
    oppBoardEl.style.opacity = '0.5';
    const msg = g.winner === 'me' ? 'You win!' :
                g.winner === 'opponent' ? 'You lost.' :
                'Game over.';
    setStatus(msg, true);
    return true;
  }
  return false;
}

async function pollIncomingDefense() {
  const s = await readStatus();
  if (!s || !s.defenseLast) return;
  const ev = s.defenseLast;
  if (!ev.n || ev.n <= lastIncomingN) return;
  lastIncomingN = ev.n;
  const k = `${ev.row},${ev.col}`;
  incomingOnMyBoard[k] = (ev.bit === 1) ? 'oppHit' : 'oppMiss';
  drawBoard(yourBoardEl, false, true);
}

function drawBoard(container, clickable, showShips = false) {
  container.innerHTML = "";
  for (let r = 0; r < 10; r++) {
    for (let c = 0; c < 10; c++) {
      const cell = document.createElement("div");
      cell.className = "cell";
      cell.dataset.r = r;
      cell.dataset.c = c;

      if (!clickable && showShips && yourBoard && yourBoard.Cells && yourBoard.Cells[r][c] === 1) {
        cell.classList.add("ship");
      }

      if (clickable) {
        cell.addEventListener("click", onShootCell);
      } else {
        cell.classList.add("disabled");
      }

      const k = `${r},${c}`;
      if (clickable) {
        if (shotState[k] === "hit")  cell.classList.add("hit");
        if (shotState[k] === "miss") cell.classList.add("miss");
      }
      if (!clickable) {
        const mk = incomingOnMyBoard && incomingOnMyBoard[k];
        if (mk === 'oppHit')  cell.classList.add('opp-hit');
        if (mk === 'oppMiss') cell.classList.add('opp-miss');
      }

      container.appendChild(cell);
    }
  }
}

async function onStartClick() {
  const oppUrl = opponentUrlInput.value.trim().replace(/\/+$/,'');
  if (!oppUrl) { setStatus("Enter opponent URL first.", false); return; }

  const myOrigin = window.location.origin.replace(/\/+$/, '');
  if (oppUrl === myOrigin) {
    setStatus("You cannot use your own URL as the opponent.", false);
    return;
  }

  opponent = { baseUrl: oppUrl, rootHex: null, vkB64: null };

  try {
    yourBoard = await postJSON('/v1/init', {});
    await postJSON('/v1/commit', { board: yourBoard });

    // remove this route later and combine it into /status
    // unused variable????
    let s1 = await putJSON('/v1/peer', { baseUrl: opponent.baseUrl });

    try {
      const oppStatus = await getJSON(`${oppUrl}/v1/status`);

      if (!oppStatus || !oppStatus.startedAt) {
        setStatus("Opponent server is not ready or returned invalid data.", false);
        return;
      }

      if (oppStatus && (oppStatus.myRootHex || oppStatus.vkB64)) {
        opponent.rootHex = oppStatus.myRootHex || null;
        opponent.vkB64   = oppStatus.vkB64   || null;

        s1 = await putJSON('/v1/peer', {
          baseUrl: opponent.baseUrl,
          rootHex: opponent.rootHex || "",
          vkB64:   opponent.vkB64   || ""
        });
      }
    } catch {
        setStatus("Opponent is offline or URL is incorrect.", false);
        return;
    }

    drawBoard(yourBoardEl, false, true);
    drawBoard(oppBoardEl, true, false);
    await refreshTurn();
    setStatus("Ready. Waiting for turns to be decided…", true);
  } catch (e) {
    setStatus(`Start failed: ${e.message}`, false);
    console.error(e);
  }
}

async function onShootCell(e) {
  const cell = e.currentTarget;
  const r = parseInt(cell.dataset.r, 10);
  const c = parseInt(cell.dataset.c, 10);
  if (!opponent || !opponent.baseUrl) { setStatus("Click Start first.", false); return; }
  if (await refreshGameState()) return;

  try {
    const s = await readStatus();
    if (!s || !s.turn || s.turn.myTurn !== 'me' || !s.turn.ready || !s.turn.decided) {
      setStatus("Not your turn.", false);
      return;
    }

    const shot = await postJSON(`${opponent.baseUrl}/v1/shoot`, { row: r, col: c });

    const rootHex = shot.rootHex || opponent.rootHex;
    const vkB64   = shot.vkB64   || opponent.vkB64;
    if (!rootHex || !vkB64) { setStatus("Missing opponent root/vk.", false); return; }

    const verify = await postJSON('/v1/verify', {
      rootHex: rootHex,
      payload: shot.payload || shot.Payload || {},
      vkB64:   vkB64
    });

    const hit = verify && (verify.Hit === 1 || verify.hit === 1);
    shotState[gridKey(r,c)] = hit ? "hit" : "miss";
    drawBoard(oppBoardEl, true, false);
    await refreshGameState();
    setStatus(hit ? `Hit (${r},${c})` : `Miss (${r},${c})`, true);

    await refreshTurn();
  } catch (e2) {
    setStatus(`Shot failed: ${e2.message}`, false);
  }
}

function persistShotState() {
  try {
    localStorage.setItem('battleship_shotState', JSON.stringify(shotState));
  } catch (err) {
    console.warn('Failed to persist shotState:', err);
  }
}

startBtn.addEventListener("click", onStartClick);
window.addEventListener('DOMContentLoaded', async () => {
  drawBoard(yourBoardEl, false, true);
  drawBoard(oppBoardEl, true, false);
  await refreshTurn();
  setInterval(refreshTurn, 1000);
  setInterval(pollIncomingDefense, 1200);
  await refreshGameState();
  setInterval(refreshGameState, 1000);
});
