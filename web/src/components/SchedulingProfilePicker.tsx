import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import type { SchedulingProfileRef } from "../api/types";
import { ErrorPanel, Field } from "./ui";

export function SchedulingProfilePicker({
  environmentId,
  value,
  onChange,
  enabled = true,
  allowClear = true,
}: {
  environmentId: string;
  value?: SchedulingProfileRef;
  onChange: (value: SchedulingProfileRef | undefined) => void;
  enabled?: boolean;
  allowClear?: boolean;
}) {
  const profiles = useQuery({
    queryKey: ["assigned-scheduling-profiles", environmentId],
    queryFn: () => api.assignedSchedulingProfiles(environmentId),
    enabled: enabled && Boolean(environmentId),
  });
  const selected = profiles.data?.items.find(
    (item) =>
      item.profileId === value?.profileId && item.revision === value.revision,
  );
  return (
    <div>
      <Field
        label="Scheduling for this service"
        hint="Optional. This selection is saved only in this service's AppConfig. Infrastructure administrators define the safe placement policies available here."
      >
        <select
          value={value ? `${value.profileId}@${value.revision}` : ""}
          disabled={!enabled || !environmentId || profiles.isLoading}
          onChange={(event) => {
            const item = profiles.data?.items.find(
              (candidate) =>
                `${candidate.profileId}@${candidate.revision}` ===
                event.target.value,
            );
            onChange(
              item
                ? {
                    profileId: item.profileId,
                    revision: item.revision,
                    specDigest: item.specDigest,
                    assignmentsDigest: item.assignmentsDigest,
                  }
                : undefined,
            );
          }}
        >
          {allowClear ? (
            <option value="">Default scheduling for this service</option>
          ) : null}
          {profiles.data?.items.map((item) => (
            <option
              key={`${item.profileId}@${item.revision}`}
              value={`${item.profileId}@${item.revision}`}
            >
              {item.name} · revision {item.revision}
            </option>
          ))}
        </select>
      </Field>
      {selected ? (
        <p className="field-hint">
          {selected.spec.description || "Administrator-managed Pod placement"}
          {Object.keys(selected.spec.pod.nodeSelector ?? {}).length
            ? ` · ${Object.keys(selected.spec.pod.nodeSelector ?? {}).length} node selector(s)`
            : ""}
          {(selected.spec.pod.tolerations?.length ?? 0) > 0
            ? ` · ${selected.spec.pod.tolerations?.length} toleration(s)`
            : ""}
        </p>
      ) : null}
      {profiles.error ? <ErrorPanel error={profiles.error} /> : null}
    </div>
  );
}
