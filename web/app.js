const $ = (sel) => document.querySelector(sel);
const yourBoardEl = $("#yourBoard");
const oppBoardEl = $("#oppBoard");
const statusEl = $("#status");
const startBtn = $("#startBtn");
const opponentUrlInput = $("#opponentUrl");
let incomingOnMyBoard = {};  // key "r,c" -> 'oppHit' | 'oppMiss'
let lastIncomingN = 0;


// local state
let yourBoard = null;      // 10x10 numbers from /v1/init
let opponent = null;       // { baseUrl, rootHex, vkB64 }
let shotState = {};        // key "r,c" -> 'hit' | 'miss'

function setStatus(text, ok = true) {
  statusEl.textContent = text;
  statusEl.style.color = ok ? "#14532d" : "#7f1d1d";
}

function gridKey(r,c) { return `${r},${c}`; }

// ---------- helpers ----------
async function postJSON(url, obj) {
  const res = await fetch(url, {
    method: "POST",
    headers: {"content-type":"application/json"},
    body: JSON.stringify(obj || {})
  });
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const data = await res.json();
      if (data && data.error) msg += `: ${data.error}`;
    } catch {}
    throw new Error(msg);
  }
  return await res.json();
}

async function getJSON(url) {
  const res = await fetch(url, { method: "GET", headers: {"content-type":"application/json"} });
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try { const d = await res.json(); if (d && d.error) msg += `: ${d.error}` } catch {}
    throw new Error(msg);
  }
  return await res.json();
}

// ---------- turns UI ----------
async function refreshTurn() {
  try {
    const t = await getJSON('/v1/turn'); // {myTurn, ready, decided, ...}
    const canClick = !!t && t.decided === true && t.ready === true && t.myTurn === 'me';

    statusEl.textContent = canClick ? "Your turn" :
                           (t && t.decided ? "Opponent’s turn" : "Deciding turns…");

    // gate clicks strictly on decided+ready+myTurn==='me'
    oppBoardEl.style.pointerEvents = canClick ? 'auto' : 'none';
    oppBoardEl.style.opacity = canClick ? '1' : '0.5';
  } catch (err) {
    // on error, be safe: disable clicking
    oppBoardEl.style.pointerEvents = 'none';
    oppBoardEl.style.opacity = '0.5';
  }
}



// ---------- boards ----------
function drawBoard(container, clickable, showShips = false) {
  container.innerHTML = "";
  for (let r = 0; r < 10; r++) {
    for (let c = 0; c < 10; c++) {
      const cell = document.createElement("div");
      cell.className = "cell";
      cell.dataset.r = r;
      cell.dataset.c = c;

      // Show my ships only on my board (left)
      if (!clickable && showShips && yourBoard && yourBoard.Cells && yourBoard.Cells[r][c] === 1) {
        cell.classList.add("ship");
      }

      // Opponent board is the only clickable board
      if (clickable) {
        cell.addEventListener("click", onShootCell);
      } else {
        cell.classList.add("disabled");
      }

      const k = `${r},${c}`;

      // ✅ Paint ONLY attacker's shots on the opponent board (right)
      if (clickable) {
        if (shotState[k] === "hit")  cell.classList.add("hit");
        if (shotState[k] === "miss") cell.classList.add("miss");
      }

      // ✅ Paint ONLY opponent's shots on my board (left)
      if (!clickable) {
        const mk = incomingOnMyBoard && incomingOnMyBoard[k];
        if (mk === 'oppHit')  cell.classList.add('opp-hit');
        if (mk === 'oppMiss') cell.classList.add('opp-miss');
      }

      container.appendChild(cell);
    }
  }
}


async function pollIncomingDefense() {
  try {
    const ev = await getJSON('/v1/defense/last'); // {n,row,col,bit,at} or {n:0}
    if (!ev || !ev.n || ev.n <= lastIncomingN) return;

    lastIncomingN = ev.n;
    const k = `${ev.row},${ev.col}`;
    incomingOnMyBoard[k] = (ev.bit === 1) ? 'oppHit' : 'oppMiss';

    // redraw your board to show new mark
    drawBoard(yourBoardEl, false, true);
  } catch (e) {
    // ignore transient errors
  }
}


async function onStartClick() {
  const oppUrl = opponentUrlInput.value.trim().replace(/\/+$/,'');
  if (!oppUrl) { setStatus("Enter opponent URL first.", false); return; }
  opponent = { baseUrl: oppUrl, rootHex: null, vkB64: null };

  try {
    // create & commit locally
    yourBoard = await postJSON('/v1/init', {});
    await postJSON('/v1/commit', { board: yourBoard });

    // identify myself (used for ID-based starter)
    await postJSON('/v1/turn/self', { baseUrl: window.location.origin });

    // IMPORTANT: tell our server the opponent ID *immediately*
    await postJSON('/v1/turn/opponent', { baseUrl: opponent.baseUrl });

    // best-effort: push our info to the opponent (optional)
    try { await postJSON('/v1/send-info', { toBaseUrl: oppUrl }); } catch {}

    // try to pull opponent info; if available, tell server again with root
    try {
      const oppInfo = await getJSON(`${oppUrl}/v1/info`);
      if (oppInfo && oppInfo.rootHex && oppInfo.vkB64) {
        opponent.rootHex = oppInfo.rootHex;
        opponent.vkB64   = oppInfo.vkB64;
        await postJSON('/v1/turn/opponent', {
          baseUrl: opponent.baseUrl,
          rootHex: opponent.rootHex
        });
      }
    } catch {}

    drawBoard(yourBoardEl, false, true);
    drawBoard(oppBoardEl, true, false);   // visually ready, but clicks will be gated below
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
  if (await refreshGameState()) return; // New

  try {
    const t = await getJSON('/v1/turn');
    if (!t || t.myTurn !== 'me') { setStatus("Not your turn.", false); return; }

    const shot = await postJSON(`${opponent.baseUrl}/v1/shoot`, { row: r, col: c });

    const rootHex = shot.rootHex || opponent.rootHex;
    const vkB64   = shot.vkB64   || opponent.vkB64;
    if (!rootHex || !vkB64) { setStatus("Missing opponent root/vk.", false); return; }

    const verify = await postJSON('/v1/verify', {
      rootHex: rootHex,
      payload: shot.payload || shot.Payload || {},
      vkB64:   vkB64
    });

    const hit = verify && verify.Hit === 1;
    shotState[`${r},${c}`] = hit ? "hit" : "miss";
    drawBoard(oppBoardEl, true, false);
    await refreshGameState(); // NEW
    setStatus(hit ? `Hit (${r},${c})` : `Miss (${r},${c})`, true);

    if (verify.Valid) {
      await postJSON('/v1/turn/next', {});
      await refreshTurn();
    }
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

// New
async function refreshGameState() {
  try {
    const g = await getJSON('/v1/game/state'); // {hitsTaken,hitsDealt,over,winner}
    if (g && g.over) {
      oppBoardEl.style.pointerEvents = 'none';
      oppBoardEl.style.opacity = '0.5';
      const msg = g.winner === 'me' ? 'You win!' :
                  g.winner === 'opponent' ? 'You lost.' :
                  'Game over.';
      setStatus(msg, true);
      return true;
    }
  } catch (e) { /* ignore */ }
  return false;
}

startBtn.addEventListener("click", onStartClick);
window.addEventListener('DOMContentLoaded', async () => {
  drawBoard(yourBoardEl, false, true);
  drawBoard(oppBoardEl, true, false);
  await refreshTurn();
  setInterval(refreshTurn, 1000);
  setInterval(pollIncomingDefense, 1200); // ~1.2s polling
  await refreshGameState(); // NEW
  setInterval(refreshGameState, 1000); // NEW: keep an eye on opponent victory

});
