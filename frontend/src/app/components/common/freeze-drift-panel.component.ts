import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { DatePipe, DecimalPipe } from '@angular/common';
import { InputDiffEntry, LayoutScenario } from '../../../types/scenario';

@Component({
  selector: 'app-freeze-drift-panel',
  standalone: true,
  imports: [DatePipe, DecimalPipe],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (scenario(); as value) {
      <section class="freeze-panel" [class.drift]="hasDrift()" [class.superseded]="value.is_superseded">
        <header>
          <div>
            <strong>{{ title() }}</strong>
            <small>Frozen at {{ value.input_frozen_at ? (value.input_frozen_at | date:'MMM d, yyyy HH:mm:ss') : 'not frozen' }}</small>
          </div>
          <span class="state" [class.ok]="!hasDrift() && !value.is_superseded" [class.bad]="hasDrift() || value.is_superseded">
            {{ value.is_superseded ? 'Superseded' : (hasDrift() ? 'Input drift' : 'Inputs consistent') }}
          </span>
        </header>

        <p class="summary">
          <strong>Drift summary:</strong> {{ value.input_diff.summary || 'No drift evidence recorded yet.' }}
          @if (value.input_diff.computed_at) {<span class="checked">checked {{ value.input_diff.computed_at | date:'MMM d, HH:mm:ss' }}</span>}
        </p>

        @if (value.is_superseded) {
          <p class="blocker">This plan was superseded by rebuild #{{ value.superseded_by_id }}. It stays here for audit but can no longer be approved.</p>
        }
        @if (value.approval_block_reason && !value.is_superseded) {
          <p class="blocker"><strong>Approval blocked:</strong> {{ value.approval_block_reason }}</p>
        }

        @if (entries().length) {
          <div class="table-wrap">
            <table>
              <thead><tr><th>Change</th><th>Entity</th><th>Field</th><th>Frozen</th><th>Current</th></tr></thead>
              <tbody>
                @for (entry of entries(); track entry.entity_type + '-' + entry.entity_id + '-' + entry.field) {
                  <tr>
                    <td><span class="pill" [class]="entry.change_type">{{ entry.change_type }}</span></td>
                    <td><strong>{{ entry.label }}</strong><small>{{ entry.entity_type }} #{{ entry.entity_id }}</small></td>
                    <td>{{ entry.field }}</td>
                    <td>{{ entry.change_type === 'changed' && isNumericField(entry.field) ? (entry.frozen | number:'1.0-3') : '—' }}</td>
                    <td>{{ entry.change_type === 'changed' && isNumericField(entry.field) ? (entry.current | number:'1.0-3') : '—' }}</td>
                  </tr>
                }
              </tbody>
            </table>
          </div>
        }
      </section>
    }
  `,
  styles: [`
    .freeze-panel{background:#fff;border:1px solid #d8dde1;border-left:4px solid #4b9a77;margin-bottom:16px}.freeze-panel.drift,.freeze-panel.superseded{border-left-color:#cf3f2e}.freeze-panel header{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:11px 14px;border-bottom:1px solid #e5e8ea}.freeze-panel header strong{display:block;font-size:12px;text-transform:uppercase}.freeze-panel header small{display:block;margin-top:3px;color:#68717a;font-size:10px}.state{font:700 10px/1 monospace;text-transform:uppercase;padding:5px 9px;border-radius:2px}.state.ok{background:#e6f4ec;color:#237a4b}.state.bad{background:#fbeae8;color:#b42318}.summary{display:flex;align-items:baseline;gap:8px;flex-wrap:wrap;padding:10px 14px;margin:0;font-size:11px;color:#4f5860}.summary .checked{margin-left:auto;color:#68717a;font-size:9px;text-transform:uppercase}.blocker{margin:0;padding:9px 14px;background:#fbeae8;color:#7a1d15;font-size:11px;line-height:1.5;border-top:1px solid #f3cfca}.table-wrap{border-top:1px solid #e5e8ea;overflow-x:auto}.table-wrap table{width:100%;border-collapse:collapse;font-size:10px}.table-wrap th{text-align:left;padding:7px 10px;color:#68717a;font:600 9px/1 monospace;text-transform:uppercase;background:#f3f5f6}.table-wrap td{padding:7px 10px;border-top:1px solid #eef0f2;vertical-align:top}.table-wrap strong,.table-wrap small{display:block}.table-wrap small{color:#68717a;font-size:9px}.pill{display:inline-block;padding:2px 7px;font:700 9px/1.4 monospace;text-transform:uppercase;border-radius:2px}.pill.added{background:#e6f4ec;color:#237a4b}.pill.missing{background:#fbeae8;color:#b42318}.pill.changed{background:#fdf3e0;color:#9a6b08}
  `]
})
export class FreezeDriftPanelComponent {
  readonly scenario = input<LayoutScenario | null>(null);
  readonly title = input('Frozen inputs & drift');
  readonly hasDrift = computed(() => {
    const diff = this.scenario()?.input_diff;
    return !!diff && (diff.added.length + diff.missing.length + diff.changed.length) > 0;
  });
  readonly entries = computed<InputDiffEntry[]>(() => {
    const diff = this.scenario()?.input_diff;
    return diff ? [...diff.added, ...diff.missing, ...diff.changed] : [];
  });

  isNumericField(field: string): boolean {
    return !['zone_status', 'load_status', 'rack_status', 'redundancy_group', 'preferred_zone_id', 'adjacency', '-'].includes(field);
  }
}
