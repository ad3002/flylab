let currentPlan = null;
let currentResolvedPlan = null;
let activeJobID = null;
let pollInterval = null;

document.addEventListener("DOMContentLoaded", () => {
  setupNavigation();
  setupFormModeToggle();
  setupSamplePrompts();
  loadHistory();
  loadCapabilities();
});

function setupNavigation() {
  document.querySelectorAll(".tab-btn").forEach(btn => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tab-btn").forEach(b => b.classList.remove("active"));
      document.querySelectorAll(".view-pane").forEach(p => p.style.display = "none");

      btn.classList.add("active");
      const targetId = btn.getAttribute("data-target");
      const targetPane = document.getElementById(targetId);
      if (targetPane) targetPane.style.display = "block";

      if (targetId === "history-view") {
        loadHistory();
      }
    });
  });
}

function setupFormModeToggle() {
  const formModeBtn = document.getElementById("mode-form-btn");
  const promptModeBtn = document.getElementById("mode-prompt-btn");
  const formSection = document.getElementById("form-section");
  const promptSection = document.getElementById("prompt-section");

  if (formModeBtn && promptModeBtn) {
    formModeBtn.addEventListener("click", () => {
      formModeBtn.classList.add("active");
      promptModeBtn.classList.remove("active");
      formSection.style.display = "block";
      promptSection.style.display = "none";
    });

    promptModeBtn.addEventListener("click", () => {
      promptModeBtn.classList.add("active");
      formModeBtn.classList.remove("active");
      formSection.style.display = "none";
      promptSection.style.display = "block";
    });
  }
}

function setupSamplePrompts() {
  document.querySelectorAll(".prompt-chip").forEach(chip => {
    chip.addEventListener("click", () => {
      const text = chip.getAttribute("data-prompt");
      const input = document.getElementById("prompt-input");
      if (input) input.value = text;
    });
  });
}

async function loadCapabilities() {
  try {
    const res = await fetch("/capabilities");
    const data = await res.json();
    const banner = document.getElementById("llm-status-text");
    if (banner) {
      banner.textContent = data.llm_ready
        ? `Local LLM Active (${data.llm_model})`
        : "Local LLM Offline (Manual Form Active)";
    }
  } catch (err) {
    console.error("Failed to load capabilities", err);
  }
}

async function parsePrompt() {
  const input = document.getElementById("prompt-input");
  const prompt = input.value.trim();
  if (!prompt) return;

  const btn = document.getElementById("parse-btn");
  btn.disabled = true;
  btn.textContent = "Parsing with LLM...";

  try {
    const res = await fetch("/api/v1/plans/parse", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        prompt: prompt,
        dataset_id: "flywire_630",
        report_language: "en"
      })
    });

    const data = await res.json();
    if (data.status === "ready") {
      currentPlan = data.plan;
      currentResolvedPlan = data.resolved_plan;
      showPlanReview(data);
    } else {
      alert(`Status: ${data.status}\nMessage: ${data.message}`);
    }
  } catch (err) {
    alert("Error parsing prompt: " + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Translate to Plan";
  }
}

async function validateFormPlan() {
  const expType = document.getElementById("exp-type").value;
  const rateHz = parseFloat(document.getElementById("rate-hz").value);
  const durationMs = parseFloat(document.getElementById("duration-ms").value);
  const repeats = parseInt(document.getElementById("repeats").value, 10);
  const baseSeed = parseInt(document.getElementById("base-seed").value, 10);
  const actGroup = document.getElementById("act-group").value;
  const roGroup = document.getElementById("ro-group").value;
  const slncGroup = document.getElementById("slnc-group").value;

  const plan = {
    schema_version: "1.0",
    dataset_id: "flywire_630",
    model_id: "shiu_lif_rust",
    experiment_type: expType,
    activation: [
      {
        selector: { group_id: actGroup },
        rate_hz: rateHz
      }
    ],
    silencing: expType === "compare_silencing" ? [{ selector: { group_id: slncGroup } }] : [],
    readout: [
      { selector: { group_id: roGroup } }
    ],
    duration_ms: durationMs,
    repeats: repeats,
    base_seed: baseSeed,
    report_language: "en"
  };

  try {
    const res = await fetch("/api/v1/plans/validate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(plan)
    });
    const data = await res.json();
    if (!res.ok) {
      alert("Validation error: " + (data.error ? data.error.message : "Unknown error"));
      return;
    }
    currentPlan = data.plan;
    currentResolvedPlan = data.resolved_plan;
    showPlanReview(data);
  } catch (err) {
    alert("Error validating plan: " + err.message);
  }
}

function showPlanReview(data) {
  // Switch to review tab
  document.querySelectorAll(".tab-btn").forEach(b => b.classList.remove("active"));
  document.querySelectorAll(".view-pane").forEach(p => p.style.display = "none");

  const reviewBtn = document.getElementById("tab-review-btn");
  const reviewPane = document.getElementById("review-view");
  if (reviewBtn) reviewBtn.classList.add("active");
  if (reviewPane) reviewPane.style.display = "block";

  document.getElementById("review-plan-id").textContent = data.resolved_plan.plan_id;
  document.getElementById("review-exp-type").textContent = data.resolved_plan.experiment_type;
  document.getElementById("review-duration").textContent = `${data.resolved_plan.duration_ms} ms`;
  document.getElementById("review-repeats").textContent = data.resolved_plan.repeats;
  document.getElementById("review-seed").textContent = data.resolved_plan.base_seed;
  document.getElementById("review-activation-count").textContent = `${data.resolved_plan.activation[0].neuron_ids.length} neurons (${data.resolved_plan.activation[0].rate_hz} Hz)`;
  document.getElementById("review-silencing-count").textContent = `${data.resolved_plan.silencing_neuron_ids.length} neurons`;
  document.getElementById("review-readout-count").textContent = `${data.resolved_plan.readout_neuron_ids.length} neurons`;

  const jsonPreview = document.getElementById("plan-json-preview");
  if (jsonPreview) {
    jsonPreview.textContent = JSON.stringify(data.resolved_plan, null, 2);
  }
}

async function launchExperiment() {
  if (!currentResolvedPlan) {
    alert("No validated plan available.");
    return;
  }

  const launchBtn = document.getElementById("launch-btn");
  launchBtn.disabled = true;

  try {
    const res = await fetch("/api/v1/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ plan_id: currentResolvedPlan.plan_id })
    });
    const data = await res.json();
    if (!res.ok) {
      alert("Failed to create job: " + (data.error ? data.error.message : "Error"));
      launchBtn.disabled = false;
      return;
    }

    activeJobID = data.job.job_id;
    startJobTracking(activeJobID);
  } catch (err) {
    alert("Launch error: " + err.message);
    launchBtn.disabled = false;
  }
}

function startJobTracking(jobId) {
  // Switch to execution view
  document.querySelectorAll(".tab-btn").forEach(b => b.classList.remove("active"));
  document.querySelectorAll(".view-pane").forEach(p => p.style.display = "none");

  const execBtn = document.getElementById("tab-exec-btn");
  const execPane = document.getElementById("execution-view");
  if (execBtn) execBtn.classList.add("active");
  if (execPane) execPane.style.display = "block";

  document.getElementById("exec-job-id").textContent = jobId;

  if (pollInterval) clearInterval(pollInterval);
  pollInterval = setInterval(() => pollJobStatus(jobId), 1000);
  pollJobStatus(jobId);
}

async function pollJobStatus(jobId) {
  try {
    const res = await fetch(`/api/v1/jobs/${jobId}`);
    if (!res.ok) return;
    const job = await res.json();

    document.getElementById("exec-status-badge").textContent = job.status.toUpperCase();
    document.getElementById("exec-status-badge").className = `status-tag status-${job.status}`;
    document.getElementById("exec-stage-text").textContent = `Stage: ${job.stage}`;

    const fill = document.getElementById("exec-progress-fill");
    if (fill) fill.style.width = `${job.progress_pct}%`;

    if (job.status === "succeeded") {
      clearInterval(pollInterval);
      loadJobResults(jobId);
    } else if (job.status === "failed" || job.status === "cancelled") {
      clearInterval(pollInterval);
      alert(`Job ended with status: ${job.status}\nError: ${job.error_message || "N/A"}`);
    }
  } catch (err) {
    console.error("Poll error", err);
  }
}

async function cancelCurrentJob() {
  if (!activeJobID) return;
  try {
    await fetch(`/api/v1/jobs/${activeJobID}/cancel`, { method: "POST" });
  } catch (err) {
    alert("Cancel request failed: " + err.message);
  }
}

async function loadJobResults(jobId) {
  // Switch to results view
  document.querySelectorAll(".tab-btn").forEach(b => b.classList.remove("active"));
  document.querySelectorAll(".view-pane").forEach(p => p.style.display = "none");

  const resBtn = document.getElementById("tab-results-btn");
  const resPane = document.getElementById("results-view");
  if (resBtn) resBtn.classList.add("active");
  if (resPane) resPane.style.display = "block";

  document.getElementById("results-export-btn").onclick = () => {
    window.location.href = `/api/v1/jobs/${jobId}/export`;
  };

  try {
    const res = await fetch(`/api/v1/jobs/${jobId}/results`);
    const data = await res.json();
    const summary = data.summary;

    const tbody = document.getElementById("results-table-body");
    tbody.innerHTML = "";

    summary.readout_summary.forEach(r => {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td class="font-mono">${r.root_id}</td>
        <td>${r.rate_A_hz.toFixed(2)} Hz</td>
        <td>${r.rate_B_hz !== undefined ? r.rate_B_hz.toFixed(2) + " Hz" : "N/A"}</td>
        <td style="color: ${r.delta_hz < 0 ? '#f85149' : (r.delta_hz > 0 ? '#3fb950' : 'inherit')}">
          ${r.delta_hz !== undefined ? r.delta_hz.toFixed(2) + " Hz" : "N/A"}
        </td>
      `;
      tbody.appendChild(tr);
    });

    document.getElementById("summary-spikes-a").textContent = summary.total_spikes_A || 0;
    document.getElementById("summary-spikes-b").textContent = summary.total_spikes_B !== undefined ? summary.total_spikes_B : "N/A";
    document.getElementById("summary-active-a").textContent = summary.active_neurons_count_A || 0;
    document.getElementById("summary-active-b").textContent = summary.active_neurons_count_B !== undefined ? summary.active_neurons_count_B : "N/A";

    renderSpikeRaster(jobId);
  } catch (err) {
    console.error("Failed to load results", err);
  }
}

async function renderSpikeRaster(jobId) {
  try {
    const res = await fetch(`/api/v1/jobs/${jobId}/spikes`);
    if (!res.ok) return;
    const csvText = await res.text();
    const lines = csvText.trim().split("\n");
    if (lines.length <= 1) return;

    const canvas = document.getElementById("raster-canvas");
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, canvas.width, canvas.height);

    // Draw baseline
    ctx.strokeStyle = "#30363d";
    ctx.strokeRect(0, 0, canvas.width, canvas.height);

    ctx.fillStyle = "#58a6ff";
    ctx.font = "11px ui-monospace";
    ctx.fillText("Readout & Active Spikes (100 ms)", 10, 20);

    const rows = lines.slice(1).map(l => l.split(","));
    rows.slice(0, 40).forEach((r, idx) => {
      const y = 35 + idx * 8;
      const count = parseInt(r[3], 10);
      const isRo = r[5] === "true";

      ctx.fillStyle = isRo ? "#f0883e" : "#8b949e";
      ctx.fillText(r[2].slice(-6), 10, y + 6);

      // Plot tick bars
      ctx.fillStyle = isRo ? "#f0883e" : "#388bfd";
      for (let s = 0; s < count; s++) {
        const x = 70 + (s * (canvas.width - 80) / Math.max(count, 10));
        ctx.fillRect(x, y, 2, 6);
      }
    });
  } catch (err) {
    console.error("Failed to render raster", err);
  }
}

async function loadHistory() {
  try {
    const res = await fetch("/api/v1/jobs?limit=20");
    const data = await res.json();
    const tbody = document.getElementById("history-table-body");
    if (!tbody) return;
    tbody.innerHTML = "";

    data.jobs.forEach(j => {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td class="font-mono">${j.job_id}</td>
        <td class="font-mono">${j.plan_id}</td>
        <td><span class="status-tag status-${j.status}">${j.status}</span></td>
        <td>${new Date(j.created_at).toLocaleTimeString()}</td>
        <td>
          <button class="btn btn-secondary" style="padding: 0.2rem 0.5rem; font-size: 0.75rem;" onclick="loadJobResults('${j.job_id}')">
            View
          </button>
        </td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    console.error("Failed to load history", err);
  }
}
