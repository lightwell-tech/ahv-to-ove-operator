import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

export interface AHVMigration extends K8sResourceCommon {
  spec: {
    source: {
      endpoint: string;
      secretRef: { name: string };
      insecure?: boolean;
      cdiProxyURL?: string;
      shutdownBeforeMigration?: boolean;
      warmMigration?: boolean;
      pauseBeforeCutover?: boolean;
    };
    vms: Array<{
      name: string;
      targetName?: string;
      guestPrepMode?: string;
      guestPrepConfig?: { sshSecretRef: { name: string }; preMigrationScript: string; timeoutSeconds?: number };
    }>;
    networkMappings?: Array<{ source: string; target: string; targetNamespace?: string }>;
    storageMappings?: Array<{
      source: string;
      targetStorageClass: string;
      accessMode?: string;
      volumeMode?: string;
    }>;
    targetNamespace?: string;
  };
  status?: {
    phase?: AHVMigrationPhase;
    totalVMs?: number;
    completedVMs?: number;
    failedVMs?: number;
    startTime?: string;
    completionTime?: string;
    conditions?: Array<{
      type: string;
      status: string;
      reason: string;
      message: string;
      lastTransitionTime: string;
    }>;
    vms?: Array<{
      name: string;
      phase?: string;
      progress?: number;
      dataVolumeRefs?: string[];
      tempImageRefs?: string[];
      ahvUUID?: string;
      vmRef?: string;
      error?: string;
    }>;
  };
}

export type AHVMigrationPhase =
  | 'Pending'
  | 'GuestPrepping'
  | 'FetchingVMInfo'
  | 'PreparingImages'
  | 'WarmPreSync'
  | 'WarmSyncing'
  | 'ReadyForCutover'
  | 'WarmCutover'
  | 'ImportingDisks'
  | 'WaitingForImport'
  | 'CreatingVMs'
  | 'Completed'
  | 'Failed';

export const AHVMigrationModel = {
  apiGroup: 'migration.lightwell.co.jp',
  apiVersion: 'v1alpha1',
  kind: 'AHVMigration',
  namespaced: true,
  plural: 'ahvmigrations',
};

export const PHASE_LABELS: Record<AHVMigrationPhase, string> = {
  Pending: '準備中',
  GuestPrepping: 'ゲスト準備中',
  FetchingVMInfo: 'VM情報取得中',
  PreparingImages: 'イメージ準備中',
  WarmPreSync: '事前コピー中',
  WarmSyncing: '同期中（VM稼働中）',
  ReadyForCutover: 'カットオーバー待機',
  WarmCutover: 'カットオーバー実行中',
  ImportingDisks: 'ディスクインポート中',
  WaitingForImport: 'インポート待機中',
  CreatingVMs: 'VM作成中',
  Completed: '完了',
  Failed: '失敗',
};

export const CUTOVER_ANNOTATION = 'migration.lightwell.co.jp/cutover-approved';
