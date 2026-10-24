import React, { useState, useEffect, useMemo } from 'react';
import {
  Terminal,
  Cpu,
  Sparkles,
  FolderTree,
  GitBranch,
  Columns,
  Rows,
  Maximize2,
  Plus,
  Settings,
  Bell,
  Activity,
  Send,
  CheckCircle2,
  Clock,
  AlertCircle,
  ChevronRight,
  ChevronDown,
  X,
  Search,
  Filter,
  Play,
  Pause,
  RefreshCw,
  Server,
  Shield,
  Sliders,
  Layers,
  Copy,
  ExternalLink,
  Zap,
  CircleDot,
  FileCode,
  GitCommit,
  Trash2,
  CornerDownLeft
} from 'lucide-react';

// --- Initial Mock Data matching user's screenshot & ecosystem ---
const INITIAL_WORKSPACES = [
  {
    id: 'ws-default',
    name: 'Default',
    path: 'C:\\Users\\nitec\\Documents\\Projects\\default',
    agents: [
      { id: 'claude-02', type: 'claude', status: 'idle', task: 'Idle — waiting for prompt', elapsed: '—', model: 'Claude 3.7 Sonnet' }
    ]
  },
  {
    id: 'ws-amux',
    name: 'amux',
    path: 'C:\\Users\\nitec\\Documents\\Projects\\amux',
    agents: [
      { id: 'codex-13', type: 'codex', status: 'busy', task: 'Integrating persistent per-terminal identity pool (500 names)', elapsed: '17s', model: 'GPT-6.1-Sol medium' },
      { id: 'claude-04', type: 'claude', status: 'busy', task: 'Refactoring agent-gateway WebSocket event backpressure', elapsed: '42s', model: 'Claude 3.7 Sonnet' },
      { id: 'term-01', type: 'terminal', status: 'idle', task: 'pwsh.exe — git status clean', elapsed: '—', model: 'Native Shell' }
    ]
  },
  {
    id: 'ws-games',
    name: 'games',
    path: 'C:\\Users\\nitec\\Documents\\Projects\\ZeroSumExpanse',
    agents: [
      { id: 'codex-19', type: 'codex', status: 'busy', task: 'Compiling UE5 compute shaders & spatial octree replication', elapsed: '1m 14s', model: 'GPT-6.1-Sol high' },
      { id: 'claude-08', type: 'claude', status: 'idle', task: 'Ready for task notifications', elapsed: '—', model: 'Claude 3.7 Sonnet' }
    ]
  },
  {
    id: 'ws-traderx',
    name: 'traderx-core',
    path: 'C:\\Users\\nitec\\Documents\\Projects\\traderx',
    agents: [
      { id: 'codex-07', type: 'codex', status: 'idle', task: 'Rithmic tick-stream latency benchmark complete', elapsed: '—', model: 'GPT-6.1-Sol medium' },
      { id: 'term-04', type: 'terminal', status: 'idle', task: 'cargo test --release passed', elapsed: '—', model: 'Native Shell' }
    ]
  }
];

const INITIAL_GATEWAY_TASKS = [
  {
    id: '01a16ed7-97e3-7220-907f-ea73c6868457',
    shortId: '01a16ed7',
    title: 'Implement persistent 500-name pool & per-terminal assignment checks',
    agent: 'codex-13',
    workspace: 'amux',
    status: 'completed',
    timestamp: '1:22 PM',
    comments: 2
  },
  {
    id: '01a16ea6-b769-71a2-bf3b-cb488ecb55c2',
    shortId: '01a16ea6',
    title: 'Streamline gateway WebSocket reconnect & event buffer pagination',
    agent: 'claude-04',
    workspace: 'amux',
    status: 'in_progress',
    timestamp: '1:34 PM',
    comments: 1
  },
  {
    id: '01a16ede-1a60-7610-b8f8-9b2ccab343bb',
    shortId: '01a16ede',
    title: 'Add workspace hover diagnostics & multi-agent busy indicator animations',
    agent: 'codex-13',
    workspace: 'amux',
    status: 'in_progress',
    timestamp: '1:36 PM',
    comments: 0
  },
  {
    id: '01a16ee4-5683-7083-8f3f-e1f93f79737a',
    shortId: '01a16ee4',
    title: 'Audit repository metadata matching rules across multi-root worktrees',
    agent: 'Unassigned',
    workspace: 'amux',
    status: 'queued',
    timestamp: '1:37 PM',
    comments: 0
  }
];

const INITIAL_GATEWAY_EVENTS = [
  {
    id: 'ev-1',
    time: '13:36:42',
    taskId: '01a16ee4-5683-7083-8f3f-e1f93f79737a',
    agent: 'codex-13',
    level: 'warn',
    message: 'Agent repository does not match the task project'
  },
  {
    id: 'ev-2',
    time: '13:36:40',
    taskId: '01a16ee4-5683-7083-8f3f-e1f93f79737a',
    agent: 'gateway',
    level: 'info',
    message: 'Received • saved to gateway task store'
  },
  {
    id: 'ev-3',
    time: '13:35:18',
    taskId: '01a16ede-1a60-7610-b8f8-9b2ccab343bb',
    agent: 'codex-15',
    level: 'info',
    message: 'Waiting for repository metadata handshake'
  },
  {
    id: 'ev-4',
    time: '13:34:05',
    taskId: '01a16ede-1a60-7610-b8f8-9b2ccab343bb',
    agent: 'codex-13',
    level: 'info',
    message: 'Submitted to native input; agent receipt and execution remain unknown'
  },
  {
    id: 'ev-5',
    time: '13:31:50',
    taskId: '01a16ea6-b769-71a2-bf3b-cb488ecb55c2',
    agent: 'claude-04',
    level: 'success',
    message: 'Agent acknowledged task payload; streaming stdout telemetry'
  },
  {
    id: 'ev-6',
    time: '13:24:53',
    taskId: '01a16ed7-97e3-7220-907f-ea73c6868457',
    agent: 'codex-13',
    level: 'success',
    message: 'Task completed: 01a16ed7-97e3-7220-907f-ea73c6868457 (Worked for 17s)'
  },
  {
    id: 'ev-7',
    time: '13:16:34',
    taskId: '01a16ed7-97e3-7220-907f-ea73c6868457',
    agent: 'defcon2-3c444710',
    level: 'info',
    message: 'New comment on task: verified pool uniqueness & position checks'
  }
];

export default function App() {
  // --- Core Navigation & Workspace State ---
  const [workspaces, setWorkspaces] = useState(INITIAL_WORKSPACES);
  const [activeWorkspaceId, setActiveWorkspaceId] = useState('ws-amux');
  const [hoveredWorkspaceId, setHoveredWorkspaceId] = useState(null);

  // --- Center Multiplexer Layout State ---
  // 'single' | 'split-right' | 'split-down'
  const [splitLayout, setSplitLayout] = useState('split-right');
  // 'terminals' | 'files' | 'git'
  const [centerMode, setCenterMode] = useState('terminals');
  const [activeTabLeft, setActiveTabLeft] = useState('codex-13');
  const [activeTabRight, setActiveTabRight] = useState('claude-04');
  const [codexInput, setCodexInput] = useState('');
  const [claudeInput, setClaudeInput] = useState('');

  // --- Terminal Scrollback Logs ---
  const [codexHistory, setCodexHistory] = useState([
    { type: 'gateway', text: '<Gateway message>New comment on task: 01a16ed7-97e3-7220-907f-ea73c6868457</Gateway message>', meta: 'Worked for 8s • 1:14 PM' },
    { type: 'gateway', text: '<Gateway message>Task completed: 01a16ed7-97e3-7220-907f-ea73c6868457</Gateway message>', meta: null },
    { type: 'bullet', text: "I'll check the completion note for the final result." },
    {
      type: 'command',
      cmd: 'Ran agent-tools tasks get 01a16ed7-97e3-7220-907f-ea73c6868457 | Select-Object -Last 4',
      output: [
        '[2026-10-24T17:12:59.144Z] defcon1-6441e063 (agent session=aca1c7b9-d271-58be-b4a3-477b4def6ba5 inst...',
        '[2026-10-24T17:16:34.551Z] defcon2-3c444710 (agent session=43b1dac2-246b-5ec9-b330-954cbcbaca58 inst...',
        '[2026-10-24T17:24:53.799Z] defcon2-3c444710 (agent session=43b1dac2-246b-5ec9-b330-954cbcbaca58 inst...',
        '+ 4 lines (ctrl+t to expand)'
      ]
    },
    {
      type: 'summary',
      text: 'Linux reports the pool complete: 500 unique names, Will Hattingh at entry 251, with count, uniqueness, and position checks passing. It\'s integrated with persistent per-terminal assignments; release work continues in the identity task.',
      tip: 'Tip: Use /title to choose what appears in your terminal\'s title.',
      meta: 'Worked for 17s • 1:22 PM'
    }
  ]);

  // --- Right Sidebar Gateway Tasks & Composer State ---
  const [gatewayTasks, setGatewayTasks] = useState(INITIAL_GATEWAY_TASKS);
  const [taskFilter, setTaskFilter] = useState('all'); // 'all' | 'active' | 'completed'
  const [newTaskText, setNewTaskText] = useState('');
  const [targetAgent, setTargetAgent] = useState('auto');
  const [taskPriority, setTaskPriority] = useState('normal');

  // --- Top-Right Popovers & Modals State ---
  const [eventsDrawerOpen, setEventsDrawerOpen] = useState(false);
  const [events, setEvents] = useState(INITIAL_GATEWAY_EVENTS);
  const [unreadEventsCount, setUnreadEventsCount] = useState(7);
  const [eventSearch, setEventSearch] = useState('');
  const [notificationsOpen, setNotificationsOpen] = useState(false);
  const [settingsModalOpen, setSettingsModalOpen] = useState(false);
  const [settingsTab, setSettingsTab] = useState('gateway'); // 'general' | 'gateway' | 'updates'
  const [newWorkspaceModalOpen, setNewWorkspaceModalOpen] = useState(false);
  const [newWsName, setNewWsName] = useState('');

  // --- Settings Preferences State ---
  const [notifyOnExit, setNotifyOnExit] = useState(true);
  const [gatewayUrl, setGatewayUrl] = useState('wss://gateway.lan.nitecon.net:8443/v1/mux');
  const [strictRepoMatch, setStrictRepoMatch] = useState(true);
  const [autoResumeAgents, setAutoResumeAgents] = useState(true);

  const activeWorkspace = useMemo(
    () => workspaces.find((w) => w.id === activeWorkspaceId) || workspaces[1],
    [workspaces, activeWorkspaceId]
  );

  // Total busy agents across all workspaces
  const totalAgentsStats = useMemo(() => {
    let total = 0;
    let busy = 0;
    workspaces.forEach((ws) => {
      ws.agents.forEach((ag) => {
        total += 1;
        if (ag.status === 'busy') busy += 1;
      });
    });
    return { total, busy };
  }, [workspaces]);

  // Toggle busy simulation so user can see animations dynamically
  const toggleSimulateBusy = (wsId, agentId) => {
    setWorkspaces((prev) =>
      prev.map((ws) => {
        if (ws.id !== wsId) return ws;
        return {
          ...ws,
          agents: ws.agents.map((ag) => {
            if (ag.id !== agentId) return ag;
            const nextStatus = ag.status === 'busy' ? 'idle' : 'busy';
            return {
              ...ag,
              status: nextStatus,
              elapsed: nextStatus === 'busy' ? '1s' : '—',
              task:
                nextStatus === 'busy'
                  ? 'Executing dispatched gateway task payload...'
                  : 'Ready for task notifications'
            };
          })
        };
      })
    );
  };

  // Spawn a new terminal/agent tab in active workspace
  const handleSpawnSession = (type) => {
    setCenterMode('terminals');
    const randomNum = Math.floor(Math.random() * 80) + 20;
    const newId = `${type === 'terminal' ? 'term' : type}-${randomNum}`;
    const newAgent = {
      id: newId,
      type,
      status: 'busy',
      task: `Initializing ${type} session in ${activeWorkspace.name}...`,
      elapsed: '2s',
      model: type === 'codex' ? 'GPT-6.1-Sol medium' : type === 'claude' ? 'Claude 3.7 Sonnet' : 'pwsh.exe'
    };

    setWorkspaces((prev) =>
      prev.map((ws) =>
        ws.id === activeWorkspaceId ? { ...ws, agents: [...ws.agents, newAgent] } : ws
      )
    );
    setActiveTabLeft(newId);

    // Log an event
    const newEv = {
      id: `ev-${Date.now()}`,
      time: new Date().toTimeString().slice(0, 8),
      taskId: 'session-spawn',
      agent: newId,
      level: 'info',
      message: `Spawned new ${type.toUpperCase()} instance attached to workspace "${activeWorkspace.name}"`
    };
    setEvents((prev) => [newEv, ...prev]);
    setUnreadEventsCount((c) => c + 1);
  };

  // Submit a new Gateway Task from the always-visible bottom right input
  const handleCreateTask = (e) => {
    e.preventDefault();
    if (!newTaskText.trim()) return;

    const hex = Math.floor(Math.random() * 0xffffffff).toString(16).padStart(8, '0');
    const fullId = `${hex}-97e3-7220-907f-ea73c6868457`;
    const assigned = targetAgent === 'auto' ? 'codex-13' : targetAgent;

    const newTask = {
      id: fullId,
      shortId: hex,
      title: newTaskText.trim(),
      agent: assigned,
      workspace: activeWorkspace.name,
      status: assigned === 'Unassigned' ? 'queued' : 'in_progress',
      timestamp: 'Just now',
      comments: 0
    };

    setGatewayTasks((prev) => [newTask, ...prev]);
    setNewTaskText('');

    // Also push event to the Events Drawer instead of cluttering the sidebar
    const newEv = {
      id: `ev-${Date.now()}`,
      time: new Date().toTimeString().slice(0, 8),
      taskId: fullId,
      agent: assigned,
      level: 'info',
      message: `Submitted to native input (${assigned}); task saved to gateway`
    };
    setEvents((prev) => [newEv, ...prev]);
    setUnreadEventsCount((c) => c + 1);

    // Reflect in Codex terminal if assigned to codex-13
    if (assigned === 'codex-13' || assigned === 'Broadcast') {
      setCodexHistory((prev) => [
        ...prev,
        {
          type: 'gateway',
          text: `<Gateway message>New task dispatched: ${fullId} — "${newTask.title}"</Gateway message>`,
          meta: 'Dispatched via Gateway • Just now'
        }
      ]);
    }
  };

  // Send prompt directly inside the Codex terminal pane
  const handleCodexPromptSubmit = (e) => {
    e.preventDefault();
    if (!codexInput.trim()) return;
    setCodexHistory((prev) => [
      ...prev,
      {
        type: 'bullet',
        text: `> ${codexInput.trim()}`
      },
      {
        type: 'summary',
        text: `Processing instruction on agent codex-13... synced state with gateway task queue.`,
        meta: 'Working • 1s'
      }
    ]);
    setCodexInput('');
  };

  const handleAddWorkspace = (e) => {
    e.preventDefault();
    if (!newWsName.trim()) return;
    const id = `ws-${Date.now()}`;
    const created = {
      id,
      name: newWsName.trim(),
      path: `C:\\Users\\nitec\\Documents\\Projects\\${newWsName.trim()}`,
      agents: [
        {
          id: `codex-${Math.floor(Math.random() * 50) + 30}`,
          type: 'codex',
          status: 'idle',
          task: 'Ready for task notifications',
          elapsed: '—',
          model: 'GPT-6.1-Sol medium'
        }
      ]
    };
    setWorkspaces((prev) => [...prev, created]);
    setActiveWorkspaceId(id);
    setNewWsName('');
    setNewWorkspaceModalOpen(false);
  };

  const filteredTasks = useMemo(() => {
    if (taskFilter === 'active') return gatewayTasks.filter((t) => t.status !== 'completed');
    if (taskFilter === 'completed') return gatewayTasks.filter((t) => t.status === 'completed');
    return gatewayTasks;
  }, [gatewayTasks, taskFilter]);

  const filteredEvents = useMemo(() => {
    if (!eventSearch.trim()) return events;
    const q = eventSearch.toLowerCase();
    return events.filter(
      (ev) =>
        ev.message.toLowerCase().includes(q) ||
        ev.agent.toLowerCase().includes(q) ||
        ev.taskId.toLowerCase().includes(q)
    );
  }, [events, eventSearch]);

  return (
    <div className="flex flex-col h-screen w-screen bg-[#0b0f17] text-slate-200 font-sans select-none overflow-hidden">
      {/* =====================================================================
          1. TOP WINDOW HEADER & GLOBAL TELEMETRY BAR
         ===================================================================== */}
      <header className="h-12 bg-[#0f1520] border-b border-slate-800/80 px-4 flex items-center justify-between shrink-0 z-30">
        {/* Left: Brand Identity + Current Project Path */}
        <div className="flex items-center space-x-3">
          <div className="flex items-center space-x-2">
            <div className="w-7 h-7 rounded-md bg-gradient-to-br from-cyan-400 to-teal-600 flex items-center justify-center shadow-sm shadow-cyan-500/20">
              <Terminal className="w-4 h-4 text-slate-950 stroke-[2.5]" />
            </div>
            <span className="font-bold tracking-tight text-slate-100 text-sm">
              amux
            </span>
            <span className="text-xs text-slate-500 hidden sm:inline">
              — agent workspaces
            </span>
          </div>

          <div className="h-4 w-[1px] bg-slate-800 mx-1 hidden md:block" />

          {/* Active Workspace Path Pill */}
          <div className="hidden md:flex items-center space-x-2 bg-slate-900/90 border border-slate-800 px-2.5 py-1 rounded text-xs font-mono text-slate-400">
            <FolderTree className="w-3.5 h-3.5 text-cyan-400 shrink-0" />
            <span className="truncate max-w-[320px]">{activeWorkspace.path}</span>
          </div>
        </div>

        {/* Center: Global Multi-Agent Status Summary Pill */}
        <div className="hidden lg:flex items-center space-x-3 bg-[#131b29] border border-slate-800/90 px-3 py-1 rounded-full text-xs">
          <div className="flex items-center space-x-1.5">
            <span className="relative flex h-2 w-2">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
              <span className="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
            </span>
            <span className="text-slate-300 font-medium">Gateway: Connected</span>
          </div>
          <span className="text-slate-700">•</span>
          <span className="text-slate-400">
            <strong className="text-slate-200">{totalAgentsStats.total}</strong> Agents
          </span>
          <span className="text-slate-700">•</span>
          <div className="flex items-center space-x-1.5 text-cyan-400 font-medium">
            <RefreshCw className={`w-3 h-3 ${totalAgentsStats.busy > 0 ? 'animate-spin' : ''}`} />
            <span>{totalAgentsStats.busy} Busy</span>
          </div>
        </div>

        {/* Right: Events Drawer Trigger, Notifications Bell, and Unified Settings Gear */}
        <div className="flex items-center space-x-2">
          {/* NEW: Dedicated Gateway Events Viewer Button (replaces raw text wall in sidebar) */}
          <button
            onClick={() => {
              setEventsDrawerOpen(!eventsDrawerOpen);
              setUnreadEventsCount(0);
              setNotificationsOpen(false);
            }}
            className={`relative flex items-center space-x-2 px-3 py-1.5 rounded-md text-xs font-medium transition-all border ${
              eventsDrawerOpen
                ? 'bg-cyan-500/15 text-cyan-300 border-cyan-500/40 shadow-sm shadow-cyan-500/10'
                : 'bg-slate-900/90 hover:bg-slate-800 text-slate-300 border-slate-800'
            }`}
            title="View Gateway Telemetry & Raw Agent Events"
          >
            <Activity className="w-3.5 h-3.5 text-cyan-400" />
            <span>Events</span>
            {unreadEventsCount > 0 && (
              <span className="px-1.5 py-0.2 bg-cyan-500 text-slate-950 font-bold rounded-full text-[10px] leading-tight">
                {unreadEventsCount}
              </span>
            )}
          </button>

          {/* App Update Notifications Bell */}
          <div className="relative">
            <button
              onClick={() => {
                setNotificationsOpen(!notificationsOpen);
                setEventsDrawerOpen(false);
              }}
              className={`p-1.5 rounded-md border transition-colors relative ${
                notificationsOpen
                  ? 'bg-slate-800 text-cyan-400 border-slate-700'
                  : 'bg-slate-900/90 hover:bg-slate-800 text-slate-400 hover:text-slate-200 border-slate-800'
              }`}
              title="Application Updates & Alerts"
            >
              <Bell className="w-4 h-4" />
              <span className="absolute -top-0.5 -right-0.5 w-2 h-2 rounded-full bg-amber-400 ring-2 ring-[#0f1520]" />
            </button>

            {/* Notifications Popover */}
            {notificationsOpen && (
              <div className="absolute right-0 mt-2 w-80 bg-[#131b29] border border-slate-700/90 rounded-lg shadow-2xl p-3 z-50">
                <div className="flex items-center justify-between pb-2 mb-2 border-b border-slate-800">
                  <span className="text-xs font-semibold uppercase tracking-wider text-slate-400">
                    Notifications & Updates
                  </span>
                  <button
                    onClick={() => setNotificationsOpen(false)}
                    className="text-slate-500 hover:text-slate-300"
                  >
                    <X className="w-3.5 h-3.5" />
                  </button>
                </div>
                <div className="space-y-2">
                  <div className="p-2.5 rounded-md bg-amber-500/10 border border-amber-500/20 text-xs">
                    <div className="flex items-center justify-between font-medium text-amber-300 mb-1">
                      <span>amux v0.4.2 Update Ready</span>
                      <span className="text-[10px] bg-amber-500/20 px-1.5 py-0.5 rounded">New</span>
                    </div>
                    <p className="text-slate-400 text-[11px] leading-relaxed mb-2">
                      Includes persistent 500-name terminal identity pool and lower gateway reconnect jitter.
                    </p>
                    <button
                      onClick={() => {
                        setNotificationsOpen(false);
                        setSettingsTab('updates');
                        setSettingsModalOpen(true);
                      }}
                      className="w-full py-1 bg-amber-400 hover:bg-amber-300 text-slate-950 font-semibold rounded text-center transition-colors"
                    >
                      Review & Restart to Update
                    </button>
                  </div>
                  <div className="p-2 rounded bg-slate-900/60 border border-slate-800/80 text-[11px] text-slate-400 flex items-center space-x-2">
                    <CheckCircle2 className="w-3.5 h-3.5 text-emerald-400 shrink-0" />
                    <span>Previous terminals reopened; agent tabs restored via native resume.</span>
                  </div>
                </div>
              </div>
            )}
          </div>

          {/* Top-Right Settings Gear Icon */}
          <button
            onClick={() => setSettingsModalOpen(true)}
            className="p-1.5 rounded-md bg-slate-900/90 hover:bg-slate-800 text-slate-400 hover:text-cyan-400 border border-slate-800 transition-colors"
            title="Unified Settings (App, Updates & Gateway Preferences)"
          >
            <Settings className="w-4 h-4" />
          </button>
        </div>
      </header>

      {/* =====================================================================
          MAIN 3-COLUMN WORKSPACE BODY
         ===================================================================== */}
      <div className="flex-1 flex min-h-0 relative">
        {/* -------------------------------------------------------------------
            2. LEFT SIDEBAR: WORKSPACES + MULTI-AGENT BUSY ANIMATIONS + HOVER
           ------------------------------------------------------------------- */}
        <aside className="w-64 bg-[#0e131d] border-r border-slate-800/80 flex flex-col justify-between shrink-0 z-20">
          <div className="p-3 flex-1 flex flex-col min-h-0">
            {/* Workspaces Section Header */}
            <div className="flex items-center justify-between mb-2.5 px-1">
              <span className="text-[11px] font-bold uppercase tracking-wider text-slate-400">
                Workspaces
              </span>
              <button
                onClick={() => setNewWorkspaceModalOpen(true)}
                className="flex items-center space-x-1 text-[11px] font-medium text-cyan-400 hover:text-cyan-300 bg-cyan-500/10 hover:bg-cyan-500/20 border border-cyan-500/20 px-2 py-0.5 rounded transition-colors"
              >
                <Plus className="w-3 h-3" />
                <span>New</span>
              </button>
            </div>

            {/* Workspaces List */}
            <div className="space-y-1.5 overflow-y-auto pr-0.5 flex-1">
              {workspaces.map((ws) => {
                const isSelected = ws.id === activeWorkspaceId;
                const busyAgents = ws.agents.filter((a) => a.status === 'busy');
                const isBusy = busyAgents.length > 0;

                return (
                  <div
                    key={ws.id}
                    className="relative group"
                    onMouseEnter={() => setHoveredWorkspaceId(ws.id)}
                    onMouseLeave={() => setHoveredWorkspaceId(null)}
                  >
                    <button
                      onClick={() => setActiveWorkspaceId(ws.id)}
                      className={`w-full text-left px-3 py-2.5 rounded-lg border transition-all flex items-center justify-between ${
                        isSelected
                          ? 'bg-gradient-to-r from-cyan-500/20 to-teal-500/5 border-cyan-500/50 text-white shadow-sm'
                          : 'bg-[#131a27]/70 hover:bg-[#172030] border-slate-800/80 text-slate-300'
                      }`}
                    >
                      {/* Left: Status Indicator + Workspace Name */}
                      <div className="flex items-center space-x-2.5 min-w-0">
                        {isBusy ? (
                          /* Animated Multi-Agent Busy Indicator */
                          <div className="relative flex items-center justify-center w-4 h-4 shrink-0">
                            <span className="animate-ping absolute inline-flex h-3.5 w-3.5 rounded-full bg-cyan-400 opacity-60"></span>
                            <span className="relative inline-flex rounded-full h-2.5 w-2.5 bg-cyan-400 shadow-sm shadow-cyan-300"></span>
                          </div>
                        ) : (
                          /* Idle Indicator */
                          <div className="w-4 h-4 flex items-center justify-center shrink-0">
                            <span
                              className={`w-2 h-2 rounded-full ${
                                isSelected ? 'bg-emerald-400' : 'border border-slate-600'
                              }`}
                            />
                          </div>
                        )}

                        <div className="truncate">
                          <div className="text-xs font-semibold tracking-tight truncate flex items-center space-x-1.5">
                            <span>{ws.name}</span>
                          </div>
                          <div className="text-[10px] text-slate-500 truncate">
                            {ws.agents.length} {ws.agents.length === 1 ? 'agent' : 'agents'}
                          </div>
                        </div>
                      </div>

                      {/* Right: Multi-Agent Activity Pill & Equalizer Bars */}
                      <div className="flex items-center space-x-1.5 shrink-0">
                        {isBusy ? (
                          <div className="flex items-center space-x-1.5 bg-cyan-500/15 border border-cyan-500/30 px-2 py-0.5 rounded-full">
                            {/* Animated activity bars */}
                            <div className="flex items-end space-x-0.5 h-2.5">
                              <span className="w-0.5 h-2 bg-cyan-400 animate-pulse rounded-full" />
                              <span className="w-0.5 h-2.5 bg-cyan-300 animate-bounce rounded-full" />
                              <span className="w-0.5 h-1.5 bg-cyan-400 animate-pulse rounded-full" />
                            </div>
                            <span className="text-[10px] font-mono font-semibold text-cyan-300">
                              {busyAgents.length}/{ws.agents.length} busy
                            </span>
                          </div>
                        ) : (
                          <span className="text-[10px] font-mono text-slate-500 bg-slate-900/80 px-1.5 py-0.5 rounded">
                            idle
                          </span>
                        )}
                      </div>
                    </button>

                    {/* HOVER DIAGNOSTIC POPOVER: Shows exact agents working in this workspace */}
                    {hoveredWorkspaceId === ws.id && (
                      <div className="fixed left-64 ml-2 -mt-12 w-80 bg-[#131b2b] border border-slate-700 rounded-xl shadow-2xl p-3 z-50 pointer-events-none animate-in fade-in duration-150">
                        <div className="flex items-center justify-between pb-2 mb-2 border-b border-slate-800">
                          <div>
                            <div className="text-xs font-bold text-white flex items-center space-x-1.5">
                              <span>Workspace: {ws.name}</span>
                              {isBusy && (
                                <span className="text-[10px] px-1.5 py-0.2 bg-cyan-500/20 text-cyan-300 rounded font-mono">
                                  ACTIVE
                                </span>
                              )}
                            </div>
                            <div className="text-[10px] font-mono text-slate-400 truncate max-w-[260px]">
                              {ws.path}
                            </div>
                          </div>
                        </div>

                        <div className="text-[10px] font-semibold uppercase tracking-wider text-slate-400 mb-1.5">
                          Assigned Terminal Identities ({ws.agents.length})
                        </div>

                        <div className="space-y-1.5">
                          {ws.agents.map((ag) => (
                            <div
                              key={ag.id}
                              className={`p-2 rounded-lg border text-xs ${
                                ag.status === 'busy'
                                  ? 'bg-cyan-950/30 border-cyan-500/30'
                                  : 'bg-slate-900/70 border-slate-800/80'
                              }`}
                            >
                              <div className="flex items-center justify-between mb-1">
                                <span className="font-mono font-semibold text-slate-200 flex items-center space-x-1.5">
                                  <span
                                    className={`w-2 h-2 rounded-full ${
                                      ag.status === 'busy'
                                        ? 'bg-cyan-400 animate-ping'
                                        : 'bg-slate-600'
                                    }`}
                                  />
                                  <span>{ag.id}</span>
                                </span>
                                <span
                                  className={`text-[10px] font-mono px-1.5 py-0.2 rounded ${
                                    ag.status === 'busy'
                                      ? 'bg-cyan-500/20 text-cyan-300 font-semibold'
                                      : 'bg-slate-800 text-slate-400'
                                  }`}
                                >
                                  {ag.status === 'busy' ? `WORKING • ${ag.elapsed}` : 'IDLE'}
                                </span>
                              </div>
                              <p className="text-[11px] text-slate-300 line-clamp-2 leading-snug">
                                {ag.task}
                              </p>
                              <div className="text-[10px] text-slate-500 mt-1 font-mono">
                                {ag.model}
                              </div>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>

            {/* Interactive Prototype Helper: Simulate Agent Activity Card */}
            <div className="mt-3 p-2.5 rounded-lg bg-[#121926] border border-slate-800/90">
              <div className="flex items-center justify-between text-[11px] font-semibold text-slate-300 mb-1.5">
                <span className="flex items-center space-x-1.5">
                  <Zap className="w-3.5 h-3.5 text-amber-400" />
                  <span>Test Busy Telemetry</span>
                </span>
                <span className="text-[10px] text-slate-500 font-mono">Hover ws to inspect</span>
              </div>
              <div className="flex flex-wrap gap-1">
                {activeWorkspace.agents.map((ag) => (
                  <button
                    key={ag.id}
                    onClick={() => toggleSimulateBusy(activeWorkspace.id, ag.id)}
                    className={`px-2 py-1 rounded text-[10px] font-mono border transition-all flex items-center space-x-1 ${
                      ag.status === 'busy'
                        ? 'bg-cyan-500/20 border-cyan-500/40 text-cyan-300'
                        : 'bg-slate-900 border-slate-800 text-slate-400 hover:text-slate-200'
                    }`}
                  >
                    <span
                      className={`w-1.5 h-1.5 rounded-full ${
                        ag.status === 'busy' ? 'bg-cyan-400' : 'bg-slate-600'
                      }`}
                    />
                    <span>{ag.id}</span>
                  </button>
                ))}
              </div>
            </div>
          </div>

          {/* Bottom-Left Footer: Unified Settings Gear + Quick Exit Notification Toggle */}
          <div className="p-3 border-t border-slate-800/80 bg-[#0b0f17]/60 flex items-center justify-between">
            <button
              onClick={() => setSettingsModalOpen(true)}
              className="flex items-center space-x-2 text-xs font-medium text-slate-300 hover:text-cyan-400 bg-slate-900 hover:bg-slate-800/90 border border-slate-800 px-3 py-1.5 rounded-md transition-colors w-full justify-between group"
            >
              <div className="flex items-center space-x-2">
                <Settings className="w-3.5 h-3.5 text-slate-400 group-hover:text-cyan-400 transition-colors" />
                <span>Settings & Gateway</span>
              </div>
              <span className="text-[10px] font-mono text-slate-500">Ctrl+,</span>
            </button>
          </div>
        </aside>

        {/* -------------------------------------------------------------------
            3. CENTER PANE: AGENT LAUNCHER, WINDOW SNAPPING & SPLIT TERMINALS
           ------------------------------------------------------------------- */}
        <main className="flex-1 flex flex-col min-w-0 bg-[#090d14]">
          {/* Center Action Toolbar: Spawn Agents + Future Views (Files/Git) + Split Controls */}
          <div className="h-11 bg-[#0e141f] border-b border-slate-800/80 px-3 flex items-center justify-between gap-2 shrink-0">
            {/* Left group: Spawn / Resume Agent Session Buttons */}
            <div className="flex items-center space-x-1.5 overflow-x-auto py-1">
              <span className="text-[10px] font-mono uppercase tracking-wider text-slate-500 mr-1 hidden xl:inline">
                Launch:
              </span>
              <button
                onClick={() => handleSpawnSession('terminal')}
                className="flex items-center space-x-1.5 px-2.5 py-1 rounded bg-slate-800/90 hover:bg-slate-700 text-slate-200 border border-slate-700/80 text-xs font-medium transition-colors"
              >
                <Terminal className="w-3.5 h-3.5 text-emerald-400" />
                <span>+ Terminal</span>
              </button>
              <button
                onClick={() => handleSpawnSession('codex')}
                className="flex items-center space-x-1.5 px-2.5 py-1 rounded bg-cyan-500/15 hover:bg-cyan-500/25 text-cyan-300 border border-cyan-500/30 text-xs font-medium transition-colors"
              >
                <Cpu className="w-3.5 h-3.5 text-cyan-400" />
                <span>+ Codex</span>
              </button>
              <button
                onClick={() => handleSpawnSession('claude')}
                className="flex items-center space-x-1.5 px-2.5 py-1 rounded bg-amber-500/15 hover:bg-amber-500/25 text-amber-300 border border-amber-500/30 text-xs font-medium transition-colors"
              >
                <Sparkles className="w-3.5 h-3.5 text-amber-400" />
                <span>+ Claude</span>
              </button>

              <div className="h-4 w-[1px] bg-slate-800 mx-1.5" />

              {/* Future-Ready Views: Files & Git */}
              <button
                onClick={() => setCenterMode(centerMode === 'files' ? 'terminals' : 'files')}
                className={`flex items-center space-x-1.5 px-2.5 py-1 rounded text-xs font-medium border transition-colors ${
                  centerMode === 'files'
                    ? 'bg-cyan-500 text-slate-950 border-cyan-400 font-semibold'
                    : 'bg-slate-900/90 hover:bg-slate-800 text-slate-400 border-slate-800'
                }`}
              >
                <FolderTree className="w-3.5 h-3.5" />
                <span>Files</span>
              </button>
              <button
                onClick={() => setCenterMode(centerMode === 'git' ? 'terminals' : 'git')}
                className={`flex items-center space-x-1.5 px-2.5 py-1 rounded text-xs font-medium border transition-colors ${
                  centerMode === 'git'
                    ? 'bg-cyan-500 text-slate-950 border-cyan-400 font-semibold'
                    : 'bg-slate-900/90 hover:bg-slate-800 text-slate-400 border-slate-800'
                }`}
              >
                <GitBranch className="w-3.5 h-3.5" />
                <span>Git</span>
              </button>
            </div>

            {/* Right group: Window Split / Snap Controls */}
            <div className="flex items-center space-x-1 bg-slate-900 p-0.5 rounded-md border border-slate-800 shrink-0">
              <button
                onClick={() => {
                  setCenterMode('terminals');
                  setSplitLayout('single');
                }}
                className={`flex items-center space-x-1 px-2 py-1 rounded text-[11px] font-medium transition-colors ${
                  splitLayout === 'single' && centerMode === 'terminals'
                    ? 'bg-slate-800 text-cyan-400 shadow-sm'
                    : 'text-slate-400 hover:text-slate-200'
                }`}
                title="Single Maximize Pane"
              >
                <Maximize2 className="w-3 h-3" />
                <span className="hidden sm:inline">Single</span>
              </button>
              <button
                onClick={() => {
                  setCenterMode('terminals');
                  setSplitLayout('split-right');
                }}
                className={`flex items-center space-x-1 px-2 py-1 rounded text-[11px] font-medium transition-colors ${
                  splitLayout === 'split-right' && centerMode === 'terminals'
                    ? 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30 shadow-sm'
                    : 'text-slate-400 hover:text-slate-200'
                }`}
                title="Split Right (Side-by-Side Codex + Claude)"
              >
                <Columns className="w-3 h-3" />
                <span>Split right</span>
              </button>
              <button
                onClick={() => {
                  setCenterMode('terminals');
                  setSplitLayout('split-down');
                }}
                className={`flex items-center space-x-1 px-2 py-1 rounded text-[11px] font-medium transition-colors ${
                  splitLayout === 'split-down' && centerMode === 'terminals'
                    ? 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30 shadow-sm'
                    : 'text-slate-400 hover:text-slate-200'
                }`}
                title="Split Down (Stacked Terminals)"
              >
                <Rows className="w-3 h-3" />
                <span>Split down</span>
              </button>
            </div>
          </div>

          {/* Center Content Area: Either Files Preview, Git Preview, or Split Terminal Multiplexer */}
          {centerMode === 'files' ? (
            <div className="flex-1 p-5 overflow-y-auto bg-[#090d14]">
              <div className="max-w-3xl mx-auto bg-[#0f1623] border border-slate-800 rounded-xl p-4">
                <div className="flex items-center justify-between pb-3 mb-3 border-b border-slate-800">
                  <div className="flex items-center space-x-2">
                    <FolderTree className="w-4 h-4 text-cyan-400" />
                    <span className="text-sm font-semibold text-white">
                      Workspace Explorer — {activeWorkspace.name}
                    </span>
                  </div>
                  <button
                    onClick={() => setCenterMode('terminals')}
                    className="text-xs text-cyan-400 hover:underline"
                  >
                    Back to Split Terminals
                  </button>
                </div>
                <div className="space-y-1 font-mono text-xs text-slate-300">
                  <div className="flex items-center justify-between p-2 rounded hover:bg-slate-800/60">
                    <span className="flex items-center space-x-2">
                      <FileCode className="w-4 h-4 text-cyan-400" />
                      <span>src/identity/pool_names.rs</span>
                    </span>
                    <span className="text-[11px] text-emerald-400">Modified by codex-13</span>
                  </div>
                  <div className="flex items-center justify-between p-2 rounded hover:bg-slate-800/60">
                    <span className="flex items-center space-x-2">
                      <FileCode className="w-4 h-4 text-amber-400" />
                      <span>src/gateway/ws_client.rs</span>
                    </span>
                    <span className="text-[11px] text-amber-400">Locked by claude-04</span>
                  </div>
                  <div className="flex items-center justify-between p-2 rounded hover:bg-slate-800/60">
                    <span className="flex items-center space-x-2">
                      <FileCode className="w-4 h-4 text-slate-500" />
                      <span>src/ui/workspace_panel.rs</span>
                    </span>
                    <span className="text-[11px] text-slate-500">Clean</span>
                  </div>
                </div>
              </div>
            </div>
          ) : centerMode === 'git' ? (
            <div className="flex-1 p-5 overflow-y-auto bg-[#090d14]">
              <div className="max-w-3xl mx-auto bg-[#0f1623] border border-slate-800 rounded-xl p-4">
                <div className="flex items-center justify-between pb-3 mb-3 border-b border-slate-800">
                  <div className="flex items-center space-x-2">
                    <GitBranch className="w-4 h-4 text-emerald-400" />
                    <span className="text-sm font-semibold text-white">
                      Git Worktree Status — main
                    </span>
                  </div>
                  <button
                    onClick={() => setCenterMode('terminals')}
                    className="text-xs text-cyan-400 hover:underline"
                  >
                    Back to Split Terminals
                  </button>
                </div>
                <div className="space-y-2 text-xs font-mono">
                  <div className="p-2.5 rounded bg-slate-900 border border-slate-800 flex items-center justify-between">
                    <span className="flex items-center space-x-2 text-slate-200">
                      <GitCommit className="w-4 h-4 text-cyan-400" />
                      <span>feat(identity): add 500 unique names pool with Will Hattingh at entry 251</span>
                    </span>
                    <span className="text-slate-500">codex-13 • 2m ago</span>
                  </div>
                  <div className="p-2.5 rounded bg-slate-900 border border-slate-800 flex items-center justify-between">
                    <span className="flex items-center space-x-2 text-slate-200">
                      <GitCommit className="w-4 h-4 text-emerald-400" />
                      <span>fix(gateway): preserve per-terminal assignment state on reconnect</span>
                    </span>
                    <span className="text-slate-500">claude-04 • 19m ago</span>
                  </div>
                </div>
              </div>
            </div>
          ) : (
            /* MULTI-PANE TERMINAL CONTAINER */
            <div
              className={`flex-1 flex min-h-0 ${
                splitLayout === 'split-down' ? 'flex-col' : 'flex-row'
              }`}
            >
              {/* ==================== PANE 1: CODEX-13 ==================== */}
              <div className="flex-1 flex flex-col min-w-0 min-h-0 border-r border-b border-slate-800/80 bg-[#070a0f]">
                {/* Pane Header Bar with Session Tabs & Unique Terminal Identity */}
                <div className="h-9 bg-[#0d131d] border-b border-slate-800/80 px-2.5 flex items-center justify-between shrink-0">
                  <div className="flex items-center space-x-1.5 overflow-x-auto">
                    {activeWorkspace.agents.map((ag) => (
                      <button
                        key={ag.id}
                        onClick={() => setActiveTabLeft(ag.id)}
                        className={`flex items-center space-x-1.5 px-2.5 py-1 rounded text-xs font-mono transition-colors ${
                          activeTabLeft === ag.id
                            ? 'bg-cyan-500 text-slate-950 font-bold'
                            : 'bg-slate-900 text-slate-400 hover:text-slate-200'
                        }`}
                      >
                        <span
                          className={`w-1.5 h-1.5 rounded-full ${
                            ag.status === 'busy'
                              ? activeTabLeft === ag.id
                                ? 'bg-slate-950 animate-ping'
                                : 'bg-cyan-400 animate-ping'
                              : 'bg-emerald-400'
                          }`}
                        />
                        <span>{ag.type === 'codex' ? 'Codex' : ag.type === 'claude' ? 'Claude' : 'Term'}</span>
                        <span className="opacity-75 text-[10px]">({ag.id})</span>
                      </button>
                    ))}
                  </div>

                  {/* Unique Terminal Identity Badge for Task Routing */}
                  <div className="flex items-center space-x-2 text-[11px] font-mono">
                    <span className="px-2 py-0.5 rounded bg-slate-900 border border-slate-800 text-cyan-400">
                      ID: {activeTabLeft} • {activeWorkspace.name}
                    </span>
                  </div>
                </div>

                {/* Sub-bar showing Notification Readiness */}
                <div className="px-3 py-1 bg-[#0a0f18] border-b border-slate-900 flex items-center justify-between text-[11px] text-slate-500 font-mono">
                  <span>Ready for task notifications</span>
                  <span className="text-emerald-400">● Attached to agent-gateway</span>
                </div>

                {/* Terminal Scrollback Output (Matching user's screenshot) */}
                <div className="flex-1 p-3.5 overflow-y-auto font-mono text-xs space-y-3 text-slate-300 leading-relaxed">
                  {codexHistory.map((item, idx) => {
                    if (item.type === 'gateway') {
                      return (
                        <div key={idx} className="space-y-1">
                          <div className="text-slate-200">
                            <span className="text-slate-500 mr-1.5">&gt;</span>
                            <span className="text-cyan-300">{item.text}</span>
                          </div>
                          {item.meta && (
                            <div className="text-slate-500 pl-4 text-[11px]">{item.meta}</div>
                          )}
                        </div>
                      );
                    }
                    if (item.type === 'bullet') {
                      return (
                        <div key={idx} className="text-slate-200">
                          • {item.text}
                        </div>
                      );
                    }
                    if (item.type === 'command') {
                      return (
                        <div key={idx} className="space-y-1">
                          <div>
                            • Ran{' '}
                            <span className="text-cyan-400">{item.cmd}</span>
                          </div>
                          <div className="pl-4 border-l border-slate-800 text-slate-400 text-[11px] space-y-0.5">
                            {item.output.map((line, lIdx) => (
                              <div key={lIdx} className="truncate">
                                {line}
                              </div>
                            ))}
                          </div>
                        </div>
                      );
                    }
                    if (item.type === 'summary') {
                      return (
                        <div key={idx} className="space-y-1.5">
                          <div className="text-slate-200">
                            • Linux reports the pool complete: 500 unique names,{' '}
                            <span className="text-emerald-400 font-semibold">Will Hattingh</span> at
                            entry 251, with count, uniqueness, and position checks passing. It's
                            integrated with persistent per-terminal assignments; release work
                            continues in the identity task.
                          </div>
                          {item.tip && (
                            <div className="pl-3 text-slate-400 text-[11px]">
                              └ <span className="text-cyan-400">Tip:</span> Use{' '}
                              <code className="text-slate-200">/title</code> to choose what appears
                              in your terminal's title.
                            </div>
                          )}
                          {item.meta && (
                            <div className="text-slate-500 text-[11px] pt-1">{item.meta}</div>
                          )}
                        </div>
                      );
                    }
                    return null;
                  })}
                </div>

                {/* Bottom Native Prompt Input inside Codex Pane */}
                <form
                  onSubmit={handleCodexPromptSubmit}
                  className="p-2.5 bg-[#0b1018] border-t border-slate-800/80 font-mono text-xs"
                >
                  <div className="flex items-center space-x-2 bg-[#070a0f] border border-slate-800 focus-within:border-cyan-500/50 rounded px-2.5 py-1.5">
                    <span className="text-cyan-400 font-bold">&gt;</span>
                    <input
                      type="text"
                      value={codexInput}
                      onChange={(e) => setCodexInput(e.target.value)}
                      placeholder="Ask Codex to do anything..."
                      className="bg-transparent flex-1 text-slate-200 focus:outline-none placeholder:text-slate-600"
                    />
                    <CornerDownLeft className="w-3.5 h-3.5 text-slate-600" />
                  </div>
                  <div className="flex items-center justify-between text-[11px] text-slate-500 mt-1.5 px-1">
                    <div>
                      <span className="text-amber-300/90">GPT-6.1-Sol medium</span> •{' '}
                      <span>~\Documents\Projects\amux</span> •{' '}
                      <span className="text-rose-400/90">weekly 94% left</span>
                    </div>
                    <div className="hidden sm:block">← for agents • ? for shortcuts</div>
                  </div>
                </form>
              </div>

              {/* ==================== PANE 2: CLAUDE-04 (Visible in Split Right / Down) ==================== */}
              {splitLayout !== 'single' && (
                <div className="flex-1 flex flex-col min-w-0 min-h-0 bg-[#070a0f]">
                  {/* Second Pane Header */}
                  <div className="h-9 bg-[#0d131d] border-b border-slate-800/80 px-2.5 flex items-center justify-between shrink-0">
                    <div className="flex items-center space-x-2">
                      <span className="flex items-center space-x-1.5 px-2.5 py-1 rounded text-xs font-mono bg-amber-500/20 text-amber-300 border border-amber-500/30 font-semibold">
                        <span className="w-1.5 h-1.5 rounded-full bg-amber-400 animate-ping" />
                        <span>Claude ({activeTabRight})</span>
                      </span>
                      <span className="text-[11px] font-mono text-slate-500">
                        Snapped {splitLayout === 'split-right' ? 'Right' : 'Down'}
                      </span>
                    </div>

                    <div className="flex items-center space-x-2">
                      <span className="px-2 py-0.5 rounded bg-slate-900 border border-slate-800 text-amber-300 text-[11px] font-mono">
                        ID: {activeTabRight} • {activeWorkspace.name}
                      </span>
                      <button
                        onClick={() => setSplitLayout('single')}
                        className="text-slate-500 hover:text-slate-300 p-0.5"
                        title="Close Split Pane"
                      >
                        <X className="w-3.5 h-3.5" />
                      </button>
                    </div>
                  </div>

                  {/* Second Pane Terminal Output */}
                  <div className="flex-1 p-3.5 overflow-y-auto font-mono text-xs space-y-3 text-slate-300 leading-relaxed">
                    <div className="text-amber-300/90">
                      &gt; &lt;Gateway message&gt;Assigned task 01a16ea6: Streamline gateway WebSocket reconnect&lt;/Gateway message&gt;
                    </div>
                    <div className="text-slate-300">
                      • Inspecting <code className="text-cyan-300">agent-gateway</code> event stream backpressure and moving verbose telemetry out of the primary task dispatch sidebar.
                    </div>
                    <div className="p-2.5 rounded bg-slate-900/90 border border-slate-800 text-[11px] text-slate-400 space-y-1">
                      <div className="text-emerald-400 font-semibold">
                        ✓ Diff applied to src/ui/right_panel.rs (+48, -112)
                      </div>
                      <div>• Pinned Task Composer input box to bottom of Gateway column</div>
                      <div>• Routed raw receipt logs to top-right Events Drawer</div>
                    </div>
                    <div className="flex items-center space-x-2 text-cyan-400 text-[11px]">
                      <RefreshCw className="w-3.5 h-3.5 animate-spin" />
                      <span>Running cargo check --workspace (elapsed: 42s)...</span>
                    </div>
                  </div>

                  {/* Second Pane Prompt Input */}
                  <div className="p-2.5 bg-[#0b1018] border-t border-slate-800/80 font-mono text-xs">
                    <div className="flex items-center space-x-2 bg-[#070a0f] border border-slate-800 focus-within:border-amber-500/50 rounded px-2.5 py-1.5">
                      <span className="text-amber-400 font-bold">&gt;</span>
                      <input
                        type="text"
                        value={claudeInput}
                        onChange={(e) => setClaudeInput(e.target.value)}
                        placeholder="Message Claude (claude-04)..."
                        className="bg-transparent flex-1 text-slate-200 focus:outline-none placeholder:text-slate-600"
                      />
                      <CornerDownLeft className="w-3.5 h-3.5 text-slate-600" />
                    </div>
                    <div className="flex items-center justify-between text-[11px] text-slate-500 mt-1.5 px-1">
                      <span>Claude 3.7 Sonnet • ~\Documents\Projects\amux</span>
                      <span className="text-amber-400/90">● Busy working</span>
                    </div>
                  </div>
                </div>
              )}
            </div>
          )}
        </main>

        {/* -------------------------------------------------------------------
            4. RIGHT SIDEBAR: DE-CLUTTERED GATEWAY TASKS + PINNED TASK COMPOSER
           ------------------------------------------------------------------- */}
        <aside className="w-80 bg-[#0e131d] border-l border-slate-800/80 flex flex-col justify-between shrink-0 z-20">
          {/* Top Header: Gateway Tasks & Quick Filter */}
          <div className="p-3.5 border-b border-slate-800/80">
            <div className="flex items-center justify-between mb-2">
              <span className="text-xs font-bold uppercase tracking-wider text-slate-300 flex items-center space-x-1.5">
                <Server className="w-3.5 h-3.5 text-cyan-400" />
                <span>Gateway Tasks</span>
              </span>
              <button
                onClick={() => setEventsDrawerOpen(true)}
                className="text-[11px] text-cyan-400 hover:text-cyan-300 flex items-center space-x-1"
              >
                <span>Raw Events ({events.length})</span>
                <ChevronRight className="w-3 h-3" />
              </button>
            </div>

            {/* Clean Status Summary */}
            <div className="text-[11px] text-slate-400 mb-2.5 flex items-center justify-between bg-slate-900/90 px-2.5 py-1.5 rounded border border-slate-800/80">
              <span>Connected • {activeWorkspace.agents.length} local agents</span>
              <span className="text-cyan-400 font-mono">
                {gatewayTasks.filter((t) => t.status !== 'completed').length} active
              </span>
            </div>

            {/* Filter Pills */}
            <div className="grid grid-cols-3 gap-1 bg-slate-900 p-1 rounded-md border border-slate-800 text-[11px]">
              {['all', 'active', 'completed'].map((f) => (
                <button
                  key={f}
                  onClick={() => setTaskFilter(f)}
                  className={`py-1 rounded capitalize font-medium transition-colors ${
                    taskFilter === f
                      ? 'bg-slate-800 text-cyan-300 shadow-sm'
                      : 'text-slate-400 hover:text-slate-200'
                  }`}
                >
                  {f}
                </button>
              ))}
            </div>
          </div>

          {/* Middle Scrollable Section: Structured Task Cards (No longer drowned out by raw event logs!) */}
          <div className="flex-1 overflow-y-auto p-3 space-y-2 min-h-0">
            {filteredTasks.map((task) => (
              <div
                key={task.id}
                className="p-2.5 rounded-lg bg-[#131b29] hover:bg-[#172030] border border-slate-800/90 transition-colors space-y-1.5"
              >
                <div className="flex items-center justify-between">
                  <span className="font-mono text-[11px] text-cyan-400 font-semibold">
                    #{task.shortId}
                  </span>
                  <span
                    className={`text-[10px] font-mono px-1.5 py-0.5 rounded uppercase font-semibold ${
                      task.status === 'in_progress'
                        ? 'bg-cyan-500/20 text-cyan-300 border border-cyan-500/30'
                        : task.status === 'completed'
                        ? 'bg-emerald-500/15 text-emerald-300 border border-emerald-500/25'
                        : 'bg-amber-500/15 text-amber-300 border border-amber-500/25'
                    }`}
                  >
                    {task.status.replace('_', ' ')}
                  </span>
                </div>

                <p className="text-xs text-slate-200 font-medium leading-snug">
                  {task.title}
                </p>

                <div className="flex items-center justify-between pt-1 text-[10px] text-slate-400 font-mono">
                  <span className="px-1.5 py-0.5 rounded bg-slate-900 border border-slate-800 text-slate-300">
                    → {task.agent}
                  </span>
                  <span>
                    Project: <strong className="text-slate-300">{task.workspace}</strong> •{' '}
                    {task.timestamp}
                  </span>
                </div>
              </div>
            ))}
          </div>

          {/* BOTTOM PINNED SECTION: Always-Visible Gateway Task Composer Input Box */}
          <form
            onSubmit={handleCreateTask}
            className="p-3 bg-[#111826] border-t border-slate-800 space-y-2.5 shrink-0 shadow-lg"
          >
            <div className="flex items-center justify-between">
              <label className="text-[11px] font-bold uppercase tracking-wider text-cyan-400 flex items-center space-x-1">
                <Send className="w-3 h-3" />
                <span>Dispatch New Gateway Task</span>
              </label>
              <span className="text-[10px] font-mono text-slate-500">
                Project: {activeWorkspace.name}
              </span>
            </div>

            {/* Target Agent Selector */}
            <div className="flex items-center space-x-1.5">
              <span className="text-[11px] text-slate-400 shrink-0">Target:</span>
              <select
                value={targetAgent}
                onChange={(e) => setTargetAgent(e.target.value)}
                className="flex-1 bg-slate-900 border border-slate-700/90 rounded px-2 py-1 text-xs font-mono text-slate-200 focus:outline-none focus:border-cyan-500"
              >
                <option value="auto">Auto-route ({activeWorkspace.name})</option>
                {activeWorkspace.agents.map((ag) => (
                  <option key={ag.id} value={ag.id}>
                    {ag.id} ({ag.type} — {ag.status})
                  </option>
                ))}
                <option value="Unassigned">Queue Unassigned</option>
                <option value="Broadcast">Broadcast to Workspace</option>
              </select>
            </div>

            {/* Always-Visible Task Input Textarea */}
            <textarea
              rows={3}
              value={newTaskText}
              onChange={(e) => setNewTaskText(e.target.value)}
              placeholder="Describe a task to submit to the gateway agents..."
              className="w-full bg-[#090d14] border border-slate-700/90 focus:border-cyan-400 rounded-md p-2 text-xs text-slate-100 placeholder:text-slate-500 focus:outline-none resize-none leading-relaxed"
            />

            <button
              type="submit"
              className="w-full py-2 px-3 rounded-md bg-gradient-to-r from-cyan-400 to-teal-500 hover:from-cyan-300 hover:to-teal-400 text-slate-950 font-bold text-xs flex items-center justify-center space-x-1.5 shadow-md shadow-cyan-500/15 transition-all cursor-pointer"
            >
              <Send className="w-3.5 h-3.5" />
              <span>Submit Task to Gateway</span>
            </button>
          </form>
        </aside>

        {/* ===================================================================
            SLIDE-OVER DRAWER: GATEWAY EVENTS VIEWER (Toggled from top-right)
           =================================================================== */}
        {eventsDrawerOpen && (
          <div className="absolute top-0 right-0 bottom-0 w-[440px] max-w-full bg-[#101725] border-l border-slate-700 shadow-2xl z-40 flex flex-col">
            <div className="p-3.5 border-b border-slate-800 flex items-center justify-between bg-[#131b2c]">
              <div className="flex items-center space-x-2">
                <Activity className="w-4 h-4 text-cyan-400" />
                <div>
                  <h3 className="text-xs font-bold uppercase tracking-wider text-white">
                    Gateway Events Telemetry
                  </h3>
                  <p className="text-[11px] text-slate-400">
                    Raw agent receipts, repository handshakes & state logs
                  </p>
                </div>
              </div>
              <button
                onClick={() => setEventsDrawerOpen(false)}
                className="p-1 rounded-md text-slate-400 hover:text-white hover:bg-slate-800"
              >
                <X className="w-4 h-4" />
              </button>
            </div>

            {/* Search & Clear bar */}
            <div className="p-2.5 border-b border-slate-800 flex items-center space-x-2 bg-[#0d131f]">
              <div className="flex-1 flex items-center space-x-1.5 bg-slate-900 border border-slate-800 rounded px-2.5 py-1">
                <Search className="w-3.5 h-3.5 text-slate-500" />
                <input
                  type="text"
                  value={eventSearch}
                  onChange={(e) => setEventSearch(e.target.value)}
                  placeholder="Filter by agent (codex-13), task UUID, or message..."
                  className="bg-transparent text-xs text-slate-200 focus:outline-none w-full"
                />
              </div>
              <button
                onClick={() => setEvents([])}
                className="px-2 py-1 text-[11px] text-slate-400 hover:text-rose-400 bg-slate-900 border border-slate-800 rounded"
              >
                Clear
              </button>
            </div>

            {/* Event Stream List */}
            <div className="flex-1 overflow-y-auto p-3 space-y-2 font-mono text-xs">
              {filteredEvents.length === 0 ? (
                <div className="text-center text-slate-500 py-12">
                  No matching gateway events.
                </div>
              ) : (
                filteredEvents.map((ev) => (
                  <div
                    key={ev.id}
                    className="p-2.5 rounded-lg bg-[#0b0f18] border border-slate-800/90 space-y-1"
                  >
                    <div className="flex items-center justify-between text-[10px]">
                      <span
                        className={`px-1.5 py-0.5 rounded font-semibold ${
                          ev.level === 'warn'
                            ? 'bg-amber-500/20 text-amber-300'
                            : ev.level === 'success'
                            ? 'bg-emerald-500/20 text-emerald-300'
                            : 'bg-cyan-500/15 text-cyan-300'
                        }`}
                      >
                        {ev.agent}
                      </span>
                      <span className="text-slate-500">{ev.time}</span>
                    </div>
                    <div className="text-[11px] text-slate-200 leading-snug">
                      {ev.message}
                    </div>
                    <div className="text-[10px] text-slate-500 truncate">
                      Task: {ev.taskId}
                    </div>
                  </div>
                ))
              )}
            </div>
          </div>
        )}
      </div>

      {/* =====================================================================
          UNIFIED SETTINGS MODAL (Combines General Preferences + Gateway + Updates)
         ===================================================================== */}
      {settingsModalOpen && (
        <div className="fixed inset-0 bg-black/70 backdrop-blur-xs flex items-center justify-center p-4 z-50">
          <div className="w-full max-w-xl bg-[#111826] border border-slate-700 rounded-xl shadow-2xl overflow-hidden">
            {/* Modal Header */}
            <div className="px-5 py-3.5 bg-[#151e2f] border-b border-slate-800 flex items-center justify-between">
              <div className="flex items-center space-x-2">
                <Settings className="w-4 h-4 text-cyan-400" />
                <h2 className="text-sm font-bold text-white">
                  amux Settings & Gateway Preferences
                </h2>
              </div>
              <button
                onClick={() => setSettingsModalOpen(false)}
                className="text-slate-400 hover:text-white p-1"
              >
                <X className="w-4 h-4" />
              </button>
            </div>

            {/* Modal Tabs */}
            <div className="flex border-b border-slate-800 bg-[#0e1420] px-5 space-x-4 text-xs font-medium">
              {[
                { id: 'gateway', label: 'Gateway Server' },
                { id: 'general', label: 'Workspaces & Terminals' },
                { id: 'updates', label: 'Updates (v0.4.2)' }
              ].map((t) => (
                <button
                  key={t.id}
                  onClick={() => setSettingsTab(t.id)}
                  className={`py-2.5 border-b-2 transition-colors ${
                    settingsTab === t.id
                      ? 'border-cyan-400 text-cyan-300'
                      : 'border-transparent text-slate-400 hover:text-slate-200'
                  }`}
                >
                  {t.label}
                </button>
              ))}
            </div>

            {/* Modal Content */}
            <div className="p-5 space-y-4 text-xs">
              {settingsTab === 'gateway' && (
                <div className="space-y-4">
                  <div>
                    <label className="block text-slate-300 font-semibold mb-1">
                      Agent Gateway Endpoint URL
                    </label>
                    <input
                      type="text"
                      value={gatewayUrl}
                      onChange={(e) => setGatewayUrl(e.target.value)}
                      className="w-full bg-slate-900 border border-slate-700 rounded px-3 py-2 font-mono text-slate-200 focus:outline-none focus:border-cyan-400"
                    />
                    <p className="text-[11px] text-slate-500 mt-1">
                      Dedicated server coordinating multi-agent task queues (github.com/nitecon/agent-gateway).
                    </p>
                  </div>

                  <div className="flex items-center justify-between p-3 rounded-lg bg-slate-900/90 border border-slate-800">
                    <div>
                      <div className="font-semibold text-slate-200">
                        Enforce Repository Metadata Matching
                      </div>
                      <div className="text-[11px] text-slate-400">
                        Prevent agents from picking up tasks whose project root does not match the active repo.
                      </div>
                    </div>
                    <input
                      type="checkbox"
                      checked={strictRepoMatch}
                      onChange={(e) => setStrictRepoMatch(e.target.checked)}
                      className="h-4 w-4 accent-cyan-400"
                    />
                  </div>
                </div>
              )}

              {settingsTab === 'general' && (
                <div className="space-y-3">
                  <div className="flex items-center justify-between p-3 rounded-lg bg-slate-900/90 border border-slate-800">
                    <div>
                      <div className="font-semibold text-slate-200">
                        Notify on process exit
                      </div>
                      <div className="text-[11px] text-slate-400">
                        Trigger desktop & header badge alert if a background Codex or Claude session terminates.
                      </div>
                    </div>
                    <input
                      type="checkbox"
                      checked={notifyOnExit}
                      onChange={(e) => setNotifyOnExit(e.target.checked)}
                      className="h-4 w-4 accent-cyan-400"
                    />
                  </div>

                  <div className="flex items-center justify-between p-3 rounded-lg bg-slate-900/90 border border-slate-800">
                    <div>
                      <div className="font-semibold text-slate-200">
                        Auto-resume native agent conversations on startup
                      </div>
                      <div className="text-[11px] text-slate-400">
                        Reopen previous terminals and restore persistent terminal identities automatically.
                      </div>
                    </div>
                    <input
                      type="checkbox"
                      checked={autoResumeAgents}
                      onChange={(e) => setAutoResumeAgents(e.target.checked)}
                      className="h-4 w-4 accent-cyan-400"
                    />
                  </div>
                </div>
              )}

              {settingsTab === 'updates' && (
                <div className="p-4 rounded-lg bg-slate-900 border border-slate-800 space-y-2">
                  <div className="flex items-center justify-between">
                    <span className="font-bold text-amber-300">amux v0.4.2 Available</span>
                    <span className="text-[11px] font-mono text-slate-400">Current: v0.4.1</span>
                  </div>
                  <p className="text-slate-400 leading-relaxed">
                    Ready to install. Terminal sessions and gateway task assignments will automatically resume after restart.
                  </p>
                </div>
              )}
            </div>

            <div className="px-5 py-3 bg-[#0e1420] border-t border-slate-800 flex justify-end space-x-2">
              <button
                onClick={() => setSettingsModalOpen(false)}
                className="px-4 py-1.5 rounded bg-cyan-400 hover:bg-cyan-300 text-slate-950 font-bold text-xs"
              >
                Save & Close
              </button>
            </div>
          </div>
        </div>
      )}

      {/* =====================================================================
          NEW WORKSPACE MODAL
         ===================================================================== */}
      {newWorkspaceModalOpen && (
        <div className="fixed inset-0 bg-black/70 flex items-center justify-center p-4 z-50">
          <form
            onSubmit={handleAddWorkspace}
            className="w-full max-w-md bg-[#111826] border border-slate-700 rounded-xl p-5 space-y-4 shadow-2xl"
          >
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-bold text-white">Create New Workspace</h3>
              <button
                type="button"
                onClick={() => setNewWorkspaceModalOpen(false)}
                className="text-slate-400 hover:text-white"
              >
                <X className="w-4 h-4" />
              </button>
            </div>
            <div>
              <label className="block text-xs text-slate-300 mb-1">Workspace Name</label>
              <input
                type="text"
                value={newWsName}
                onChange={(e) => setNewWsName(e.target.value)}
                placeholder="e.g., agent-gateway"
                className="w-full bg-slate-900 border border-slate-700 rounded px-3 py-2 text-xs text-white focus:outline-none focus:border-cyan-400"
                autoFocus
              />
            </div>
            <div className="flex justify-end space-x-2">
              <button
                type="button"
                onClick={() => setNewWorkspaceModalOpen(false)}
                className="px-3 py-1.5 rounded bg-slate-800 text-slate-300 text-xs"
              >
                Cancel
              </button>
              <button
                type="submit"
                className="px-3 py-1.5 rounded bg-cyan-400 text-slate-950 font-bold text-xs"
              >
                Create Workspace
              </button>
            </div>
          </form>
        </div>
      )}
    </div>
  );
}