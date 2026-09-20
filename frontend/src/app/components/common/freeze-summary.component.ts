import { ChangeDetectionStrategy, Component, computed, input, signal } from '@angular/core';
import { DatePipe, SlicePipe } from '@angular/common';
import { MatButtonModule } from '@angular/material/button';
import { LucideAngularModule } from 'lucide-angular';
import { LayoutScenario } from '../../../types/scenario';
import { ConstraintBadgeComponent } from './constraint-badge.component';

@Component({
  selector: 'app-freeze-summary',
  standalone: true,
  imports: [DatePipe, SlicePipe, MatButtonModule, LucideAngularModule, ConstraintBadgeComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (scenario(); as value) {
      <section class="freeze-panel" [class.drifted]="hasDrift()" [class.clear]="!hasDrift()">
        <header>
          <span class="freeze-title"><lucide-icon [name]="hasDrift() ? 'snowflake' : 'lock'" [size]="15" /><strong>Input freeze</strong></span>
          <app-constraint-badge
            [severity]="hasDrift() ? 'critical' : 'clear'"
            [label]="hasDrift() ? 'Approval blocked by input drift' : 'Inputs match frozen snapshot'" />
        </header>
        <dl class="freeze-meta">
          <div><dt>Frozen at</dt><dd>{{ value.frozen_at ? (value.frozen_at | date:'MMM d, yyyy HH:mm:ss') : 'Not frozen' }}</dd></div>
          <div><dt>Algorithm</dt><dd>{{ value.algorithm_version }}</dd></div>
          @if (value.source_scenario_id) {<div><dt>Rebuilt from</dt><dd>scenario #{{ value.source_scenario_id }}</dd></div>}
        </dl>
        @if (hasDrift(); as drifted) {
          <div class="drift-summary">
            <p class="blocking-reason"><lucide-icon name="shield-alert" [size]="14" />{{ value.input_drift?.blocking_reason || 'Frozen inputs no longer match current data.' }}</p>
            <ul class="drift-counts">
              <li class="added"><strong>{{ value.input_drift?.added_count ?? 0 }}</strong><span>added</span></li>
              <li class="removed"><strong>{{ value.input_drift?.removed_count ?? 0 }}</strong><span>missing</span></li>
              <li class="changed"><strong>{{ value.input_drift?.changed_count ?? 0 }}</strong><span>changed</span></li>
            </ul>
            <ul class="drift-entries">
              @for (entry of (value.input_drift?.entries ?? []) | slice:0:visibleCount(); track entry.entity_type + entry.entity_id + entry.field + entry.kind) {
                <li [class]="entry.kind">
                  <span class="kind">{{ entry.kind }}</span>
                  <span class="entity">{{ entry.entity_type }} #{{ entry.entity_id }} ({{ entry.identifier }})</span>
                  @if (entry.field) {<span class="field">{{ entry.field }}</span>}
                  <span class="detail">{{ entry.detail }}</span>
                </li>
              }
            </ul>
            @if ((value.input_drift?.total_count ?? 0) > visibleCount()) {
              <button class="expand" mat-button type="button" (click)="expanded.set(!expanded())">
                {{ expanded() ? 'Show less' : ('Show all ' + (value.input_drift?.total_count ?? 0) + ' differences') }}
              </button>
            }
            <div class="freeze-actions">
              <ng-content select="[rebuildAction]"></ng-content>
            </div>
          </div>
        }
      </section>
    }
  `,
  styles: [`
    .freeze-panel{border:1px solid #d8dde1;background:#fff;margin-bottom:16px}
    .freeze-panel.drifted{border-left:4px solid #cf3f2e}
    .freeze-panel.clear{border-left:4px solid #4b9a77}
    header{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:10px 14px;border-bottom:1px solid #e5e8ea}
    .freeze-title{display:inline-flex;align-items:center;gap:7px}.freeze-title strong{font-size:12px;text-transform:uppercase;letter-spacing:.04em}
    .freeze-meta{display:flex;flex-wrap:wrap;gap:24px;padding:10px 14px;margin:0}
    .freeze-meta dt{color:#68717a;font-size:9px;text-transform:uppercase;letter-spacing:.05em}.freeze-meta dd{margin:3px 0 0;font-size:12px;font-weight:600}
    .drift-summary{padding:0 14px 12px}
    .blocking-reason{display:flex;align-items:flex-start;gap:7px;margin:2px 0 10px;color:#a1281e;font-size:11px;font-weight:600;line-height:1.4}
    .drift-counts{display:flex;gap:8px;list-style:none;padding:0;margin:0 0 10px}
    .drift-counts li{min-width:74px;padding:7px 10px;border:1px solid #e5e8ea;text-align:center}
    .drift-counts strong{display:block;font-size:18px}.drift-counts span{font-size:9px;text-transform:uppercase;color:#68717a}
    .drift-counts li.added strong{color:#237a4b}.drift-counts li.removed strong{color:#a1281e}.drift-counts li.changed strong{color:#855900}
    .drift-entries{list-style:none;margin:0;padding:0;max-height:170px;overflow:auto;border-top:1px solid #e5e8ea}
    .drift-entries li{display:grid;grid-template-columns:64px minmax(140px,1fr) 130px;gap:8px;padding:7px 2px;border-bottom:1px solid #eef1f3;font-size:10px;align-items:baseline}
    .drift-entries li .kind{text-transform:uppercase;font-weight:750;font-size:9px}
    .drift-entries li.added .kind{color:#237a4b}.drift-entries li.removed .kind{color:#a1281e}.drift-entries li.changed .kind{color:#855900}
    .drift-entries .entity{font-weight:600}.drift-entries .field{color:#68717a;font-family:monospace;font-size:9px}
    .drift-entries .detail{grid-column:2/-1;color:#4f5860;line-height:1.4}
    .expand{font-size:10px;margin:4px 0}
    .freeze-actions{display:flex;justify-content:flex-end;margin-top:6px}
    @media(max-width:650px){.drift-entries li{grid-template-columns:56px 1fr}.drift-entries .field{grid-column:2/-1}}
  `]
})
export class FreezeSummaryComponent {
  readonly scenario = input<LayoutScenario | null>(null);
  readonly expanded = signal(false);

  readonly hasDrift = computed(() => this.scenario()?.input_drift?.has_drift ?? false);
  readonly visibleCount = computed(() => this.expanded() ? Number.MAX_SAFE_INTEGER : 6);
}
