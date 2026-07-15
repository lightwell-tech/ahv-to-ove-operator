import * as React from 'react';
import { Label } from '@patternfly/react-core';
import { AHVMigrationPhase, PHASE_LABELS } from '../types/ahvmigration';

const PHASE_COLORS: Record<string, 'blue' | 'cyan' | 'green' | 'orange' | 'red' | 'grey' | 'gold' | 'purple'> = {
  Pending: 'grey',
  GuestPrepping: 'purple',
  FetchingVMInfo: 'blue',
  PreparingImages: 'blue',
  WarmPreSync: 'cyan',
  WarmSyncing: 'cyan',
  ReadyForCutover: 'gold',
  WarmCutover: 'orange',
  ImportingDisks: 'blue',
  WaitingForImport: 'blue',
  CreatingVMs: 'purple',
  Completed: 'green',
  Failed: 'red',
};

export const PhaseLabel: React.FC<{ phase?: AHVMigrationPhase }> = ({ phase }) => {
  if (!phase) return <Label color="grey">-</Label>;
  return (
    <Label color={PHASE_COLORS[phase] ?? 'grey'}>
      {PHASE_LABELS[phase] ?? phase}
    </Label>
  );
};
