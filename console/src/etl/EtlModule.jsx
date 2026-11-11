// 数据集成模块入口：二级导航（工程 / 运行记录 / 调度 / 连接），设计器与运行详情为独立视图。
import React from 'react';
import { Tabs } from '../components/primitives.jsx';
import './etl.css';
import { ProjectsPage } from './pages/Projects.jsx';
import { RunsPage } from './pages/Runs.jsx';
import { RunDetailPage } from './pages/RunDetail.jsx';
import { SchedulesPage } from './pages/Schedules.jsx';
import { ConnectionsPage } from './pages/Connections.jsx';
import { DesignerPage } from './designer/Designer.jsx';

const TABS = [
  { id: 'projects', label: '工程', icon: 'flow' },
  { id: 'runs', label: '运行记录', icon: 'activity' },
  { id: 'schedules', label: '调度', icon: 'calendar' },
  { id: 'connections', label: '连接', icon: 'database' },
];

/**
 * route: { page: 'etl' | 'etl-designer' | 'etl-run', tab, defId, runId }
 * nav(page, opts) 由 App 提供。
 */
export function EtlModule({ route, nav }) {
  const go = {
    tab: (tab) => nav('etl', { tab }),
    designer: (defId, nodeId) => nav('etl-designer', { defId, node: nodeId }),
    run: (runId) => nav('etl-run', { runId }),
    back: (tab) => nav('etl', { tab: tab || 'projects' }),
  };
  if (route.page === 'etl-designer' && route.defId) {
    return <DesignerPage defId={route.defId} focusNodeId={route.node} onBack={() => go.back('projects')} onOpenRun={(id) => go.run(id)} />;
  }
  if (route.page === 'etl-run' && route.runId) {
    return <RunDetailPage runId={route.runId} go={go} />;
  }
  const tab = TABS.find((t) => t.id === route.tab) ? route.tab : 'projects';
  return (
    <div>
      <div className="etl-head">
        <div style={{ flex: 1 }}>
          <h2>数据集成</h2>
          <p>在数据库之间编排数据流动：作业负责「何时、按什么条件」，转换负责「数据如何流动与变换」。</p>
        </div>
      </div>
      <Tabs tabs={TABS} active={tab} onChange={go.tab} style={{ marginBottom: 18 }} />
      {tab === 'projects' ? <ProjectsPage go={go} /> : null}
      {tab === 'runs' ? <RunsPage go={go} /> : null}
      {tab === 'schedules' ? <SchedulesPage go={go} /> : null}
      {tab === 'connections' ? <ConnectionsPage /> : null}
    </div>
  );
}
