import * as React from 'react';
import { useK8sWatchResource, useActiveNamespace } from '@openshift-console/dynamic-plugin-sdk';
import {
  Badge,
  Button,
  Card,
  CardBody,
  EmptyState,
  EmptyStateBody,
  EmptyStateHeader,
  EmptyStateIcon,
  Flex,
  FlexItem,
  Page,
  PageSection,
  Progress,
  ProgressSize,
  Spinner,
  Title,
} from '@patternfly/react-core';
import {
  CheckCircleIcon,
  ExclamationCircleIcon,
  InProgressIcon,
  MigrationIcon,
  ClockIcon,
  OutlinedClockIcon,
  ServerIcon,
} from '@patternfly/react-icons';
import { useHistory } from 'react-router-dom';
import { AHVMigration, AHVMigrationModel } from '../types/ahvmigration';
import { PhaseLabel } from './PhaseLabel';
import { formatDate, elapsedTime } from '../utils/format';

const PHASE_ORDER = [
  'Pending','GuestPrepping','PreparingImages','WarmPreSync',
  'ImportingDisks','WarmSyncing','ReadyForCutover','WarmCutover','CreatingVMs','Completed',
];

function overallProgress(mig: AHVMigration): number {
  if (mig.status?.phase === 'Completed') return 100;
  if (mig.status?.phase === 'Failed') return 0;
  const vms = mig.status?.vms ?? [];
  if (vms.length === 0) {
    const idx = PHASE_ORDER.indexOf(mig.status?.phase ?? '');
    if (idx >= 0) return Math.round((idx / (PHASE_ORDER.length - 1)) * 100);
    return 0;
  }
  return Math.round(vms.reduce((s, v) => s + (v.progress ?? 0), 0) / vms.length);
}

const TimeRow: React.FC<{ icon: React.ReactNode; label: string; value: string }> = ({ icon, label, value }) => (
  <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapXs' }} style={{ fontSize: 12, color: '#6a6e73' }}>
    <FlexItem style={{ color: '#8a8d90' }}>{icon}</FlexItem>
    <FlexItem><span style={{ fontWeight: 500 }}>{label}</span> {value}</FlexItem>
  </Flex>
);

const MigrationCard: React.FC<{ mig: AHVMigration; onClick: () => void }> = ({ mig, onClick }) => {
  const phase = mig.status?.phase ?? 'Pending';
  const failed = phase === 'Failed';
  const completed = phase === 'Completed';
  const progress = overallProgress(mig);
  const vmCount = mig.status?.totalVMs ?? mig.spec.vms.length;
  const completedVMs = mig.status?.completedVMs ?? 0;

  const borderColor = failed ? '#c9190b' : completed ? '#3e8635' : '#0066cc';

  return (
    <Card
      isClickable
      onClick={onClick}
      style={{
        cursor: 'pointer',
        borderLeft: `4px solid ${borderColor}`,
        transition: 'box-shadow 0.15s',
      }}
      onMouseEnter={e => (e.currentTarget.style.boxShadow = '0 2px 8px rgba(0,0,0,0.15)')}
      onMouseLeave={e => (e.currentTarget.style.boxShadow = '')}
    >
      <CardBody style={{ padding: '16px 20px' }}>
        {/* 1行目: 名前 + フェーズ + VM数 */}
        <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapSm' }} style={{ marginBottom: 10 }}>
          <FlexItem>
            <MigrationIcon style={{ color: borderColor, verticalAlign: 'middle' }} />
          </FlexItem>
          <FlexItem flex={{ default: 'flex_1' }}>
            <span style={{ fontWeight: 600, fontSize: 15 }}>{mig.metadata?.name}</span>
            <span style={{ fontSize: 12, color: '#6a6e73', marginLeft: 8 }}>{mig.metadata?.namespace}</span>
          </FlexItem>
          <FlexItem>
            <PhaseLabel phase={phase as any} />
          </FlexItem>
          <FlexItem>
            <Badge isRead>
              <ServerIcon style={{ marginRight: 4 }} />
              {completedVMs}/{vmCount} VM
            </Badge>
          </FlexItem>
        </Flex>

        {/* 2行目: プログレスバー */}
        <div style={{ marginBottom: 10 }}>
          <Flex justifyContent={{ default: 'justifyContentSpaceBetween' }} style={{ marginBottom: 4 }}>
            <FlexItem>
              <span style={{ fontSize: 11, color: '#6a6e73' }}>進捗</span>
            </FlexItem>
            <FlexItem>
              <span style={{ fontSize: 11, fontWeight: 600, color: borderColor }}>{progress}%</span>
            </FlexItem>
          </Flex>
          <Progress
            value={progress}
            size={ProgressSize.sm}
            aria-label="migration progress"
            style={{ '--pf-v5-c-progress__indicator--BackgroundColor': borderColor } as React.CSSProperties}
          />
        </div>

        {/* 3行目: 時刻情報 */}
        <Flex gap={{ default: 'gapLg' }} flexWrap={{ default: 'wrap' }}>
          {mig.status?.startTime && (
            <FlexItem>
              <TimeRow
                icon={<ClockIcon />}
                label="開始:"
                value={formatDate(mig.status.startTime)}
              />
            </FlexItem>
          )}
          {mig.status?.completionTime ? (
            <FlexItem>
              <TimeRow
                icon={<CheckCircleIcon color="#3e8635" />}
                label="完了:"
                value={formatDate(mig.status.completionTime)}
              />
            </FlexItem>
          ) : mig.status?.startTime && (
            <FlexItem>
              <TimeRow
                icon={<OutlinedClockIcon />}
                label="経過:"
                value={elapsedTime(mig.status.startTime)}
              />
            </FlexItem>
          )}
          {failed && (
            <FlexItem>
              <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapXs' }} style={{ fontSize: 12 }}>
                <FlexItem><ExclamationCircleIcon color="#c9190b" /></FlexItem>
                <FlexItem style={{ color: '#c9190b' }}>移行失敗</FlexItem>
              </Flex>
            </FlexItem>
          )}
          {!completed && !failed && mig.status?.startTime && (
            <FlexItem style={{ marginLeft: 'auto' }}>
              <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapXs' }} style={{ fontSize: 12, color: '#0066cc' }}>
                <FlexItem><InProgressIcon /></FlexItem>
                <FlexItem>実行中</FlexItem>
              </Flex>
            </FlexItem>
          )}
        </Flex>
      </CardBody>
    </Card>
  );
};

const SummaryBadge: React.FC<{ icon: React.ReactNode; label: string; count: number; color: string }> = ({ icon, label, count, color }) => (
  <Flex
    alignItems={{ default: 'alignItemsCenter' }}
    gap={{ default: 'gapXs' }}
    style={{
      padding: '6px 14px',
      background: '#f0f0f0',
      borderRadius: 20,
      fontSize: 13,
    }}
  >
    <FlexItem style={{ color }}>{icon}</FlexItem>
    <FlexItem style={{ fontWeight: 600 }}>{count}</FlexItem>
    <FlexItem style={{ color: '#6a6e73' }}>{label}</FlexItem>
  </Flex>
);

const MigrationListPage: React.FC = () => {
  const history = useHistory();
  const [namespace] = useActiveNamespace();

  const [migrations, loaded, error] = useK8sWatchResource<AHVMigration[]>({
    groupVersionKind: {
      group: AHVMigrationModel.apiGroup,
      version: AHVMigrationModel.apiVersion,
      kind: AHVMigrationModel.kind,
    },
    namespace: namespace === '#ALL_NS#' ? undefined : namespace,
    isList: true,
  });

  if (!loaded) return <PageSection><Spinner aria-label="Loading..." /></PageSection>;

  if (error) {
    return (
      <PageSection>
        <EmptyState>
          <EmptyStateHeader title="エラー" />
          <EmptyStateBody>{String(error)}</EmptyStateBody>
        </EmptyState>
      </PageSection>
    );
  }

  const completed = migrations.filter(m => m.status?.phase === 'Completed').length;
  const failed = migrations.filter(m => m.status?.phase === 'Failed').length;
  const inProgress = migrations.filter(m =>
    m.status?.phase && !['Completed', 'Failed', 'Pending', ''].includes(m.status.phase)
  ).length;

  return (
    <Page>
      <PageSection variant="light">
        <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapMd' }} flexWrap={{ default: 'wrap' }}>
          <FlexItem>
            <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapSm' }}>
              <FlexItem>
                <MigrationIcon style={{ fontSize: 28, color: '#0066cc', verticalAlign: 'middle' }} />
              </FlexItem>
              <FlexItem>
                <Title headingLevel="h1" style={{ marginBottom: 0 }}>AHV → OVE 移行</Title>
              </FlexItem>
            </Flex>
          </FlexItem>

          {migrations.length > 0 && (
            <FlexItem>
              <Flex gap={{ default: 'gapSm' }} flexWrap={{ default: 'wrap' }}>
                <FlexItem>
                  <SummaryBadge icon={<InProgressIcon />} label="実行中" count={inProgress} color="#0066cc" />
                </FlexItem>
                <FlexItem>
                  <SummaryBadge icon={<CheckCircleIcon />} label="完了" count={completed} color="#3e8635" />
                </FlexItem>
                <FlexItem>
                  <SummaryBadge icon={<ExclamationCircleIcon />} label="失敗" count={failed} color="#c9190b" />
                </FlexItem>
              </Flex>
            </FlexItem>
          )}

          <FlexItem style={{ marginLeft: 'auto' }}>
            <Button
              variant="primary"
              onClick={() => history.push('/ahv-to-ove-plugin/create')}
            >
              移行を作成
            </Button>
          </FlexItem>
        </Flex>
      </PageSection>

      <PageSection>
        {migrations.length === 0 ? (
          <EmptyState>
            <EmptyStateHeader
              titleText="移行がありません"
              icon={<EmptyStateIcon icon={MigrationIcon} />}
            />
            <EmptyStateBody>「移行を作成」ボタンで新しい移行を開始してください。</EmptyStateBody>
          </EmptyState>
        ) : (
          <Flex direction={{ default: 'column' }} gap={{ default: 'gapMd' }}>
            {migrations
              .slice()
              .sort((a, b) => {
                const ta = a.status?.startTime ?? a.metadata?.creationTimestamp ?? '';
                const tb = b.status?.startTime ?? b.metadata?.creationTimestamp ?? '';
                return tb.localeCompare(ta);
              })
              .map((mig) => (
                <FlexItem key={`${mig.metadata?.namespace}/${mig.metadata?.name}`}>
                  <MigrationCard
                    mig={mig}
                    onClick={() =>
                      history.push(`/ahv-to-ove-plugin/ns/${mig.metadata?.namespace}/${mig.metadata?.name}`)
                    }
                  />
                </FlexItem>
              ))}
          </Flex>
        )}
      </PageSection>
    </Page>
  );
};

export default MigrationListPage;
