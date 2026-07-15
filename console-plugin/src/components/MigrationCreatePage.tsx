import * as React from 'react';
import { k8sCreate, useActiveNamespace } from '@openshift-console/dynamic-plugin-sdk';
import {
  ActionGroup,
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  Checkbox,
  Flex,
  FlexItem,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Page,
  PageSection,
  Radio,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { MinusCircleIcon, PlusCircleIcon } from '@patternfly/react-icons';
import { useHistory } from 'react-router-dom';
import { AHVMigrationModel } from '../types/ahvmigration';

interface VMEntry {
  name: string;
  targetName: string;
  uefi: 'auto' | 'true' | 'false';
  diskBus: 'auto' | 'virtio' | 'sata' | 'scsi';
  nicModel: 'auto' | 'virtio' | 'e1000e';
  keepMAC: boolean;
}

interface NetworkMapping {
  source: string;
  target: string;
  testTarget: string;
}

type MigrationMode = 'warm' | 'shutdown' | 'dataonly';

const DEFAULT_ENDPOINT = 'https://prism-central.example.com:9440';
const DEFAULT_SECRET = 'ahv-credentials';
const DEFAULT_CDI_PROXY = 'http://prism-proxy.vm-migration.svc:9440';
const DEFAULT_STORAGE_CLASS = 'ocs-storagecluster-ceph-rbd';
const DEFAULT_NETWORK_TARGET = 'example-bridge';
const DEFAULT_NETWORK_SOURCE = 'VLAN_100';

const defaultVM = (): VMEntry => ({
  name: '',
  targetName: '',
  uefi: 'auto',
  diskBus: 'auto',
  nicModel: 'auto',
  keepMAC: true,
});

const defaultNetwork = (): NetworkMapping => ({
  source: '',
  target: '',
  testTarget: '',
});

const MigrationCreatePage: React.FC = () => {
  const history = useHistory();
  const [namespace] = useActiveNamespace();
  const targetNS = namespace === '#ALL_NS#' ? 'vm-migration' : namespace;

  const [migrationName, setMigrationName] = React.useState('');
  const [endpoint, setEndpoint] = React.useState(DEFAULT_ENDPOINT);
  const [secretName, setSecretName] = React.useState(DEFAULT_SECRET);
  const [cdiProxy, setCdiProxy] = React.useState(DEFAULT_CDI_PROXY);
  const [mode, setMode] = React.useState<MigrationMode>('warm');
  const [pauseBeforeCutover, setPauseBeforeCutover] = React.useState(false);
  const [vms, setVms] = React.useState<VMEntry[]>([{ ...defaultVM(), name: '' }]);
  const [networkMappings, setNetworkMappings] = React.useState<NetworkMapping[]>([
    { source: DEFAULT_NETWORK_SOURCE, target: DEFAULT_NETWORK_TARGET, testTarget: '' },
  ]);
  const [storageClass, setStorageClass] = React.useState(DEFAULT_STORAGE_CLASS);
  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const addVM = () => setVms(v => [...v, defaultVM()]);
  const removeVM = (i: number) => setVms(v => v.filter((_, idx) => idx !== i));
  const updateVM = <K extends keyof VMEntry>(i: number, field: K, val: VMEntry[K]) =>
    setVms(v => v.map((vm, idx) => idx === i ? { ...vm, [field]: val } : vm));

  const addNetwork = () => setNetworkMappings(n => [...n, defaultNetwork()]);
  const removeNetwork = (i: number) => setNetworkMappings(n => n.filter((_, idx) => idx !== i));
  const updateNetwork = <K extends keyof NetworkMapping>(i: number, field: K, val: NetworkMapping[K]) =>
    setNetworkMappings(n => n.map((m, idx) => idx === i ? { ...m, [field]: val } : m));

  const hasTestMigration = networkMappings.some(m => m.testTarget.trim() !== '');

  const handleSubmit = async () => {
    setError(null);
    if (!migrationName) { setError('移行名を入力してください'); return; }
    if (vms.some(v => !v.name)) { setError('VM名を入力してください'); return; }

    const resource = {
      apiVersion: `${AHVMigrationModel.apiGroup}/${AHVMigrationModel.apiVersion}`,
      kind: AHVMigrationModel.kind,
      metadata: {
        name: migrationName,
        namespace: 'ahv-to-ove-operator-system',
      },
      spec: {
        source: {
          endpoint,
          secretRef: { name: secretName },
          insecure: true,
          ...(cdiProxy ? { cdiProxyURL: cdiProxy } : {}),
          warmMigration: mode === 'warm',
          shutdownBeforeMigration: mode === 'shutdown',
          pauseBeforeCutover: mode === 'warm' && pauseBeforeCutover,
        },
        vms: vms.map(v => ({
          name: v.name,
          ...(v.targetName ? { targetName: v.targetName } : {}),
          ...(v.uefi !== 'auto' ? { uefi: v.uefi === 'true' } : {}),
          ...(v.diskBus !== 'auto' ? { diskBus: v.diskBus } : {}),
          ...(v.nicModel !== 'auto' ? { nicModel: v.nicModel } : {}),
          keepMAC: v.keepMAC,
        })),
        networkMappings: networkMappings
          .filter(m => m.source || m.target)
          .map(m => ({
            source: m.source,
            target: m.target,
            ...(m.testTarget.trim() ? { testTarget: m.testTarget.trim() } : {}),
          })),
        storageMappings: [
          {
            source: '',
            targetStorageClass: storageClass,
            accessMode: 'ReadWriteMany',
            volumeMode: 'Block',
          },
        ],
        targetNamespace: targetNS,
      },
    };

    setSubmitting(true);
    try {
      await k8sCreate({ model: AHVMigrationModel as any, data: resource });
      history.push('/ahv-to-ove-plugin');
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Page>
      <PageSection variant="light">
        <Title headingLevel="h1">移行を作成</Title>
      </PageSection>

      <PageSection>
        <Form style={{ maxWidth: 900 }}>
          {error && (
            <Alert variant="danger" title={error} isInline style={{ marginBottom: 16 }} />
          )}

          {/* 基本情報 */}
          <Card style={{ marginBottom: 16 }}>
            <CardTitle>基本情報</CardTitle>
            <CardBody>
              <FormGroup label="移行名" isRequired fieldId="migration-name">
                <TextInput
                  id="migration-name"
                  value={migrationName}
                  onChange={(_e, v) => setMigrationName(v)}
                  placeholder="my-migration-01"
                />
                <div style={{ marginTop: 4, fontSize: 12, color: '#6a6e73' }}>小文字英数字とハイフンのみ</div>
              </FormGroup>
            </CardBody>
          </Card>

          {/* Prism Central接続 */}
          <Card style={{ marginBottom: 16 }}>
            <CardTitle>Prism Central 接続</CardTitle>
            <CardBody>
              <FormGroup label="エンドポイント" isRequired fieldId="endpoint">
                <TextInput
                  id="endpoint"
                  value={endpoint}
                  onChange={(_e, v) => setEndpoint(v)}
                  placeholder="https://prism-central.example.com:9440"
                />
              </FormGroup>
              <FormGroup label="認証Secret名" isRequired fieldId="secret" style={{ marginTop: 16 }}>
                <TextInput
                  id="secret"
                  value={secretName}
                  onChange={(_e, v) => setSecretName(v)}
                  placeholder="ahv-credentials"
                />
              </FormGroup>
              <FormGroup label="CDI Proxy URL" fieldId="cdi-proxy" style={{ marginTop: 16 }}>
                <TextInput
                  id="cdi-proxy"
                  value={cdiProxy}
                  onChange={(_e, v) => setCdiProxy(v)}
                  placeholder="http://prism-proxy.vm-migration.svc:9440"
                />
              </FormGroup>
            </CardBody>
          </Card>

          {/* 移行モード */}
          <Card style={{ marginBottom: 16 }}>
            <CardTitle>移行モード</CardTitle>
            <CardBody>
              <Flex direction={{ default: 'column' }} gap={{ default: 'gapMd' }}>
                <FlexItem>
                  <Radio
                    id="mode-warm"
                    name="mode"
                    label="ウォーム移行（ダウンタイム最小化）"
                    description="VM起動中にディスクをコピーし、カットオーバー時のみ短時間停止します"
                    isChecked={mode === 'warm'}
                    onChange={() => setMode('warm')}
                  />
                </FlexItem>
                <FlexItem>
                  <Radio
                    id="mode-shutdown"
                    name="mode"
                    label="停止移行"
                    description="VMをシャットダウンしてからディスクをコピーします（データ整合性が高い）"
                    isChecked={mode === 'shutdown'}
                    onChange={() => setMode('shutdown')}
                  />
                </FlexItem>
                <FlexItem>
                  <Radio
                    id="mode-dataonly"
                    name="mode"
                    label="DataOnly（SourceURL必須）"
                    description="PrismにImageが存在する場合のみ。通常はウォーム移行を使用してください"
                    isChecked={mode === 'dataonly'}
                    onChange={() => setMode('dataonly')}
                  />
                </FlexItem>
                {mode === 'warm' && (
                  <FlexItem style={{ marginLeft: 24 }}>
                    <Radio
                      id="pause-cutover"
                      name="pause"
                      label="カットオーバー前に一時停止する（手動承認）"
                      description="ReadyForCutover フェーズで停止し、コンソールから承認後に切り替えます"
                      isChecked={pauseBeforeCutover}
                      onChange={() => setPauseBeforeCutover(v => !v)}
                    />
                  </FlexItem>
                )}
              </Flex>
            </CardBody>
          </Card>

          {/* VM一覧 */}
          <Card style={{ marginBottom: 16 }}>
            <CardTitle>移行対象 VM</CardTitle>
            <CardBody>
              {vms.map((vm, i) => (
                <div key={i} style={{ marginBottom: 16, padding: 12, background: '#f0f0f0', borderRadius: 6 }}>
                  {/* 行1: VM名 / OVE名 / 削除 */}
                  <Flex gap={{ default: 'gapMd' }} alignItems={{ default: 'alignItemsFlexEnd' }}>
                    <FlexItem flex={{ default: 'flex_1' }}>
                      <FormGroup label="AHV VM名" isRequired fieldId={`vm-name-${i}`}>
                        <TextInput
                          id={`vm-name-${i}`}
                          value={vm.name}
                          onChange={(_e, v) => updateVM(i, 'name', v)}
                          placeholder="vm-name-on-ahv"
                        />
                      </FormGroup>
                    </FlexItem>
                    <FlexItem flex={{ default: 'flex_1' }}>
                      <FormGroup label="OVE上のVM名（省略可）" fieldId={`vm-target-${i}`}>
                        <TextInput
                          id={`vm-target-${i}`}
                          value={vm.targetName}
                          onChange={(_e, v) => updateVM(i, 'targetName', v)}
                          placeholder="vm-name-on-ove"
                        />
                      </FormGroup>
                    </FlexItem>
                    <FlexItem>
                      <Button
                        variant="plain"
                        onClick={() => removeVM(i)}
                        isDisabled={vms.length === 1}
                        aria-label="Remove VM"
                        style={{ marginTop: 24 }}
                      >
                        <MinusCircleIcon color="#c9190b" />
                      </Button>
                    </FlexItem>
                  </Flex>
                  {/* 行2: ファームウェア / DiskBus / NicModel / keepMAC */}
                  <Flex gap={{ default: 'gapMd' }} alignItems={{ default: 'alignItemsFlexEnd' }} style={{ marginTop: 12 }}>
                    <FlexItem style={{ minWidth: 130 }}>
                      <FormGroup label="ファームウェア" fieldId={`vm-uefi-${i}`}>
                        <FormSelect
                          id={`vm-uefi-${i}`}
                          value={vm.uefi}
                          onChange={(_e, v) => updateVM(i, 'uefi', v as VMEntry['uefi'])}
                          aria-label="ファームウェア"
                        >
                          <FormSelectOption value="auto" label="自動検出" />
                          <FormSelectOption value="true" label="UEFI" />
                          <FormSelectOption value="false" label="BIOS" />
                        </FormSelect>
                      </FormGroup>
                    </FlexItem>
                    <FlexItem style={{ minWidth: 140 }}>
                      <FormGroup label="ディスクバス" fieldId={`vm-diskbus-${i}`}>
                        <FormSelect
                          id={`vm-diskbus-${i}`}
                          value={vm.diskBus}
                          onChange={(_e, v) => updateVM(i, 'diskBus', v as VMEntry['diskBus'])}
                          aria-label="ディスクバス"
                        >
                          <FormSelectOption value="auto" label="自動（virtio）" />
                          <FormSelectOption value="virtio" label="virtio（Linux推奨）" />
                          <FormSelectOption value="sata" label="sata（Windows推奨）" />
                          <FormSelectOption value="scsi" label="scsi" />
                        </FormSelect>
                      </FormGroup>
                    </FlexItem>
                    <FlexItem style={{ minWidth: 150 }}>
                      <FormGroup label="NICモデル" fieldId={`vm-nic-${i}`}>
                        <FormSelect
                          id={`vm-nic-${i}`}
                          value={vm.nicModel}
                          onChange={(_e, v) => updateVM(i, 'nicModel', v as VMEntry['nicModel'])}
                          aria-label="NICモデル"
                        >
                          <FormSelectOption value="auto" label="自動（virtio）" />
                          <FormSelectOption value="virtio" label="virtio（Linux推奨）" />
                          <FormSelectOption value="e1000e" label="e1000e（Windows推奨）" />
                        </FormSelect>
                      </FormGroup>
                    </FlexItem>
                    <FlexItem style={{ paddingBottom: 6 }}>
                      <Checkbox
                        id={`vm-mac-${i}`}
                        label="MACアドレスを引き継ぐ"
                        isChecked={vm.keepMAC}
                        onChange={(_e, checked) => updateVM(i, 'keepMAC', checked)}
                        description="チェックを外すとKubeVirtが新しいMACを自動割り当て"
                      />
                    </FlexItem>
                  </Flex>
                </div>
              ))}
              <Button variant="link" icon={<PlusCircleIcon />} onClick={addVM}>
                VMを追加
              </Button>
            </CardBody>
          </Card>

          {/* ネットワークマッピング */}
          <Card style={{ marginBottom: 16 }}>
            <CardTitle>
              ネットワークマッピング
              {hasTestMigration && (
                <span style={{ marginLeft: 12, fontSize: 12, color: '#0066cc', fontWeight: 'normal' }}>
                  テストVLANが設定されています → TestRunning フェーズ経由でデプロイされます
                </span>
              )}
            </CardTitle>
            <CardBody>
              {/* ヘッダー行 */}
              <Flex gap={{ default: 'gapMd' }} style={{ marginBottom: 4 }}>
                <FlexItem flex={{ default: 'flex_1' }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: '#6a6e73' }}>AHV ネットワーク名</span>
                </FlexItem>
                <FlexItem flex={{ default: 'flex_1' }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: '#6a6e73' }}>本番 OVE NAD 名</span>
                </FlexItem>
                <FlexItem flex={{ default: 'flex_1' }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: '#0066cc' }}>テストVLAN NAD（省略可）</span>
                </FlexItem>
                <FlexItem style={{ minWidth: 36 }} />
              </Flex>
              {networkMappings.map((nm, i) => (
                <Flex key={i} gap={{ default: 'gapMd' }} alignItems={{ default: 'alignItemsCenter' }} style={{ marginBottom: 8 }}>
                  <FlexItem flex={{ default: 'flex_1' }}>
                    <TextInput
                      id={`net-src-${i}`}
                      value={nm.source}
                      onChange={(_e, v) => updateNetwork(i, 'source', v)}
                      placeholder="VLAN_100"
                    />
                  </FlexItem>
                  <FlexItem style={{ fontSize: 18, color: '#6a6e73' }}>→</FlexItem>
                  <FlexItem flex={{ default: 'flex_1' }}>
                    <TextInput
                      id={`net-tgt-${i}`}
                      value={nm.target}
                      onChange={(_e, v) => updateNetwork(i, 'target', v)}
                      placeholder="example-bridge"
                    />
                  </FlexItem>
                  <FlexItem flex={{ default: 'flex_1' }}>
                    <TextInput
                      id={`net-test-${i}`}
                      value={nm.testTarget}
                      onChange={(_e, v) => updateNetwork(i, 'testTarget', v)}
                      placeholder="vm-migration-test-vlan（省略可）"
                    />
                  </FlexItem>
                  <FlexItem>
                    <Button
                      variant="plain"
                      onClick={() => removeNetwork(i)}
                      isDisabled={networkMappings.length === 1}
                      aria-label="Remove network"
                    >
                      <MinusCircleIcon color="#c9190b" />
                    </Button>
                  </FlexItem>
                </Flex>
              ))}
              <Button variant="link" icon={<PlusCircleIcon />} onClick={addNetwork}>
                マッピングを追加
              </Button>
              {hasTestMigration && (
                <div style={{ marginTop: 12, padding: 8, background: '#e7f1fa', borderRadius: 4, fontSize: 12 }}>
                  テスト移行フロー: ディスクコピー完了後、テストVLANでVMを起動 → 動作確認 →
                  <code style={{ margin: '0 4px' }}>migration.lightwell.co.jp/cutover-approved: "true"</code>
                  アノテーションを付与 → 本番VLANに切り替えて完了
                </div>
              )}
            </CardBody>
          </Card>

          {/* ストレージ */}
          <Card style={{ marginBottom: 24 }}>
            <CardTitle>ストレージ</CardTitle>
            <CardBody>
              <FormGroup label="StorageClass" isRequired fieldId="storage-class">
                <TextInput
                  id="storage-class"
                  value={storageClass}
                  onChange={(_e, v) => setStorageClass(v)}
                  placeholder="ocs-storagecluster-ceph-rbd"
                />
              </FormGroup>
              <div style={{ marginTop: 8, fontSize: 12, color: '#6a6e73' }}>
                AccessMode: ReadWriteMany / VolumeMode: Block（固定）
              </div>
            </CardBody>
          </Card>

          <ActionGroup>
            <Button
              variant="primary"
              onClick={handleSubmit}
              isLoading={submitting}
              isDisabled={submitting}
            >
              移行を作成
            </Button>
            <Button variant="secondary" onClick={() => history.push('/ahv-to-ove-plugin')}>
              キャンセル
            </Button>
          </ActionGroup>
        </Form>
      </PageSection>
    </Page>
  );
};

export default MigrationCreatePage;
