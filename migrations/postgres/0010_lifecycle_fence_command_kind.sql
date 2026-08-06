-- Lifecycle stop effects insert their own rows into runner_commands, but they
-- shared kind='fence' with the assignment reconciler, whose fence and terminal
-- paths bulk-expire every ('assignment','fence') command for an assignment_id.
-- That collaterally expired lifecycle-owned commands and made the stop broker's
-- delivery guard fail on rows it no longer controlled. Lifecycle-owned fence
-- commands now carry kind='lifecycle_fence'; retag the rows the current stop
-- effects reference so the two reconcilers own disjoint rows.
--
-- Superseded retry commands are already in a terminal state, which every
-- kind-filtered query also excludes, so only the currently referenced command
-- of each stop effect needs the new kind.
UPDATE secondbox.runner_commands AS command
   SET kind = 'lifecycle_fence'
  FROM secondbox.lifecycle_effects AS effect
 WHERE effect.kind = 'stop'
   AND command.id = effect.command_id
   AND command.kind = 'fence';
