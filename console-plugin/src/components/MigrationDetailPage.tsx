import * as React from 'react';
import { useK8sWatchResource, k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
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
  PendingIcon,
  ResourcesAlmostFullIcon,
} from '@patternfly/react-icons';
import { useHistory, useParams } from 'react-router-dom';
import { AHVMigration, AHVMigrationModel, CUTOVER_ANNOTATION } from '../types/ahvmigration';
import { PhaseLabel } from './PhaseLabel';
import { formatDate } from '../utils/format';

// フェーズの順序定義
const PHASE_STEPS = [
  { key: 'Pending',          label: '準備待ち' },
  { key: 'GuestPrepping',    label: 'ゲスト準備' },
  { key: 'PreparingImages',  label: 'イメージ作成' },
  { key: 'WarmPreSync',      label: '事前同期' },
  { key: 'ImportingDisks',   label: 'ディスクコピー' },
  { key: 'WarmSyncing',      label: '同期待機' },
  { key: 'ReadyForCutover',  label: '承認待ち' },
  { key: 'WarmCutover',      label: 'カットオーバー' },
  { key: 'CreatingVMs',      label: 'VM作成' },
  { key: 'Completed',        label: '完了' },
];

// 現在の移行タイプに合わせて表示するステップを絞り込む
function getVisibleSteps(mig: AHVMigration) {
  const warm = mig.spec?.source?.warmMigration;
  const shutdown = mig.spec?.source?.shutdownBeforeMigration;
  const hasGuestPrep = mig.spec?.vms?.some(v => v.guestPrepMode === 'ssh');

  return PHASE_STEPS.filter(s => {
    if (s.key === 'GuestPrepping' && !hasGuestPrep) return false;
    if (s.key === 'PreparingImages' && !shutdown) return false;
    if (s.key === 'WarmPreSync' && !warm) return false;
    if (s.key === 'WarmSyncing' && !warm) return false;
    if (s.key === 'ReadyForCutover' && !mig.spec?.source?.pauseBeforeCutover) return false;
    if (s.key === 'WarmCutover' && !warm) return false;
    if (s.key === 'ImportingDisks' && warm) return false;
    return true;
  });
}

const PhaseStepIcon: React.FC<{ status: 'done' | 'active' | 'pending' | 'failed' }> = ({ status }) => {
  const size = 28;
  if (status === 'done')    return <CheckCircleIcon color="#3e8635" style={{ fontSize: size }} />;
  if (status === 'active')  return <InProgressIcon color="#0066cc" style={{ fontSize: size }} />;
  if (status === 'failed')  return <ExclamationCircleIcon color="#c9190b" style={{ fontSize: size }} />;
  return <PendingIcon color="#8a8d90" style={{ fontSize: size }} />;
};

const PhaseStepper: React.FC<{ mig: AHVMigration }> = ({ mig }) => {
  const phase = mig.status?.phase ?? 'Pending';
  const failed = phase === 'Failed';
  const steps = getVisibleSteps(mig);
  const currentIdx = steps.findIndex(s => s.key === phase);

  return (
    <div style={{ display: 'flex', alignItems: 'flex-start', gap: 0, overflowX: 'auto', padding: '8px 0' }}>
      {steps.map((step, idx) => {
        let status: 'done' | 'active' | 'pending' | 'failed' = 'pending';
        if (failed && idx === currentIdx) status = 'failed';
        else if (idx < currentIdx || phase === 'Completed') status = 'done';
        else if (idx === currentIdx) status = 'active';

        const isLast = idx === steps.length - 1;
        return (
          <React.Fragment key={step.key}>
            <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', minWidth: 72 }}>
              <PhaseStepIcon status={status} />
              <div style={{
                fontSize: 11,
                marginTop: 4,
                textAlign: 'center',
                color: status === 'active' ? '#0066cc' : status === 'done' ? '#3e8635' : '#6a6e73',
                fontWeight: status === 'active' ? 600 : 400,
                whiteSpace: 'nowrap',
              }}>
                {step.label}
              </div>
            </div>
            {!isLast && (
              <div style={{
                flexGrow: 1,
                height: 2,
                marginTop: 13,
                backgroundColor: idx < currentIdx ? '#3e8635' : '#d2d2d2',
                minWidth: 24,
              }} />
            )}
          </React.Fragment>
        );
      })}
    </div>
  );
};

const MigrationDetailPage: React.FC = () => {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const history = useHistory();
  const [approving, setApproving] = React.useState(false);
  const [approveError, setApproveError] = React.useState<string | null>(null);

  const [mig, loaded, error] = useK8sWatchResource<AHVMigration>({
    groupVersionKind: {
      group: AHVMigrationModel.apiGroup,
      version: AHVMigrationModel.apiVersion,
      kind: AHVMigrationModel.kind,
    },
    namespace,
    name,
    isList: false,
  });

  const handleApproveCutover = async () => {
    if (!mig) return;
    setApproving(true);
    setApproveError(null);
    try {
      const patches: { op: string; path: string; value?: unknown }[] = [];
      if (!mig.metadata?.annotations) {
        patches.push({ op: 'add', path: '/metadata/annotations', value: {} });
      }
      const annotationKey = CUTOVER_ANNOTATION.replace(/~/g, '~0').replace(/\//g, '~1');
      patches.push({ op: 'add', path: `/metadata/annotations/${annotationKey}`, value: 'true' });
      await k8sPatch({
        model: AHVMigrationModel as any,
        resource: mig,
        data: patches,
      });
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e);
      setApproveError(msg);
    } finally {
      setApproving(false);
    }
  };

  if (!loaded) {
    return <PageSection><Spinner aria-label="Loading..." /></PageSection>;
  }
  if (error || !mig) {
    return <PageSection><Alert variant="danger" title="読み込みエラー" /></PageSection>;
  }

  const phase = mig.status?.phase;
  const isReadyForCutover = phase === 'ReadyForCutover';
  const isCompleted = phase === 'Completed';
  const isFailed = phase === 'Failed';
  const alreadyApproved = mig.metadata?.annotations?.[CUTOVER_ANNOTATION] === 'true';

  const overallProgress = (() => {
    if (isCompleted) return 100;
    const vms = mig.status?.vms ?? [];
    if (vms.length === 0) return 0;
    return Math.round(vms.reduce((s, v) => s + (v.progress ?? 0), 0) / vms.length);
  })();

  return (
    <Page>
      {/* ヘッダー */}
      <PageSection variant="light" style={{ paddingBottom: 0 }}>
        <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapMd' }}>
          <FlexItem>
            <ResourcesAlmostFullIcon style={{ fontSize: 36, color: '#0066cc' }} />
          </FlexItem>
          <FlexItem flex={{ default: 'flex_1' }}>
            <Title headingLevel="h1">{name}</Title>
            <div style={{ marginTop: 4 }}>
              <PhaseLabel phase={phase as any} />
            </div>
          </FlexItem>
          {(isCompleted || overallProgress > 0) && (
            <FlexItem style={{ minWidth: 200 }}>
              <div style={{ fontSize: 12, color: '#6a6e73', marginBottom: 4 }}>全体進捗</div>
              <Progress
                value={overallProgress}
                size={ProgressSize.sm}
                title={`${overallProgress}%`}
              />
            </FlexItem>
          )}
        </Flex>
      </PageSection>

      {/* フェーズステッパー */}
      <PageSection variant="light" style={{ paddingTop: 16 }}>
        <Card isFlat>
          <CardBody>
            <PhaseStepper mig={mig} />
          </CardBody>
        </Card>
      </PageSection>

      <PageSection>
        <Flex direction={{ default: 'column' }} gap={{ default: 'gapMd' }}>

          {/* カットオーバー承認 */}
          {isReadyForCutover && (
            <FlexItem>
              <Alert
                variant="warning"
                title="カットオーバー待機中"
                actionLinks={
                  <ActionGroup>
                    <Button
                      variant="primary"
                      onClick={handleApproveCutover}
                      isLoading={approving}
                      isDisabled={approving || alreadyApproved}
                    >
                      {alreadyApproved ? '承認済み' : 'カットオーバーを承認する'}
                    </Button>
                  </ActionGroup>
                }
              >
                事前コピーが完了しました。承認するとVMがシャットダウンしてOVEに切り替わります。
                {approveError && <div style={{ color: 'red', marginTop: 8 }}>{approveError}</div>}
              </Alert>
            </FlexItem>
          )}

          {isFailed && (
            <FlexItem>
              <Alert variant="danger" title="移行失敗">
                {mig.status?.vms?.find(v => v.error)?.error ?? '詳細はConditionsを確認してください。'}
              </Alert>
            </FlexItem>
          )}

          {/* 概要 */}
          <FlexItem>
            <Card>
              <CardTitle>概要</CardTitle>
              <CardBody>
                <DescriptionList columnModifier={{ default: '2Col' }}>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Namespace</DescriptionListTerm>
                    <DescriptionListDescription>{namespace}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Prism Endpoint</DescriptionListTerm>
                    <DescriptionListDescription>{mig.spec.source.endpoint}</DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>移行モード</DescriptionListTerm>
                    <DescriptionListDescription>
                      {mig.spec.source.warmMigration
                        ? 'ウォーム移行（ダウンタイム最小化）'
                        : mig.spec.source.shutdownBeforeMigration
                        ? '停止移行（bare disk対応）'
                        : '通常移行'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>開始時刻</DescriptionListTerm>
                    <DescriptionListDescription>
                      {mig.status?.startTime ? formatDate(mig.status.startTime) : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>完了時刻</DescriptionListTerm>
                    <DescriptionListDescription>
                      {mig.status?.completionTime ? formatDate(mig.status.completionTime) : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>対象NS</DescriptionListTerm>
                    <DescriptionListDescription>
                      {mig.spec.targetNamespace ?? namespace}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </CardBody>
            </Card>
          </FlexItem>

          {/* VM 一覧 */}
          <FlexItem>
            <Card>
              <CardTitle>VM 移行状況</CardTitle>
              <CardBody>
                {(mig.status?.vms ?? mig.spec.vms.map((v) => ({ name: v.name }))).map((vmSt) => (
                  <Card key={vmSt.name} isFlat style={{ marginBottom: 12, border: '1px solid #d2d2d2' }}>
                    <CardBody>
                      <Flex alignItems={{ default: 'alignItemsCenter' }} gap={{ default: 'gapSm' }}>
                        <FlexItem flex={{ default: 'flex_1' }}>
                          <strong style={{ fontSize: 14 }}>{vmSt.name}</strong>
                        </FlexItem>
                        <FlexItem>
                          <PhaseLabel phase={(vmSt as any).phase} />
                        </FlexItem>
                      </Flex>
                      {(vmSt as any).progress !== undefined && (vmSt as any).progress > 0 && (
                        <div style={{ marginTop: 10 }}>
                          <Progress
                            value={(vmSt as any).progress}
                            size={ProgressSize.sm}
                            title={`${(vmSt as any).progress}%`}
                          />
                        </div>
                      )}
                      {(vmSt as any).error && (
                        <Alert variant="danger" isInline title={(vmSt as any).error} style={{ marginTop: 8 }} />
                      )}
                      {(vmSt as any).vmRef && (
                        <div style={{ marginTop: 6, fontSize: 12, color: '#6a6e73' }}>
                          作成VM: <code>{(vmSt as any).vmRef}</code>
                        </div>
                      )}
                    </CardBody>
                  </Card>
                ))}
              </CardBody>
            </Card>
          </FlexItem>

        </Flex>
      </PageSection>

      <PageSection>
        <Button variant="secondary" onClick={() => history.push('/ahv-to-ove-plugin')}>
          一覧に戻る
        </Button>
      </PageSection>
    </Page>
  );
};

export default MigrationDetailPage;
