import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { SharedArray } from 'k6/data';
import { Counter, Rate, Trend } from 'k6/metrics';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:3000').replace(/\/$/, '');
const DATA_FILE = __ENV.BENCHMARK_DATA_FILE || '../data.json';
const TARGET_TPS = positiveInteger('TARGET_TPS', 100);
const WARMUP_TPS = positiveInteger('WARMUP_TPS', Math.min(TARGET_TPS, 100));
const WARMUP_DURATION = __ENV.WARMUP_DURATION || '10s';
const DURATION = __ENV.DURATION || '30s';
const WARMUP_GRACEFUL_STOP = __ENV.WARMUP_GRACEFUL_STOP || '20s';
const WARMUP_SETTLE_DURATION = __ENV.WARMUP_SETTLE_DURATION || '2s';
const GRACEFUL_STOP = __ENV.GRACEFUL_STOP || '30s';
const DEFAULT_AMOUNT = __ENV.POSTING_AMOUNT || '1.00';
const RUN_ID = __ENV.BENCHMARK_RUN_ID || String(Date.now());
// Five milliseconds keeps the observer's quantization below the 50 ms SLO.
// The previous 50 ms interval imposed a 55+ ms floor even when projection and
// transfer processing completed much earlier.
const POLL_INTERVAL_SECONDS = positiveNumber('POLL_INTERVAL_SECONDS', 0.01);
const PROJECTION_TIMEOUT_SECONDS = positiveNumber('PROJECTION_TIMEOUT_SECONDS', 15);
const TRANSFER_TIMEOUT_SECONDS = positiveNumber('TRANSFER_TIMEOUT_SECONDS', 15);
const POLL_TRANSFER_COMPLETION = (__ENV.POLL_TRANSFER_COMPLETION || 'false').toLowerCase() === 'true';

if (TARGET_TPS < 4) {
  throw new Error('TARGET_TPS must be at least 4 so every measured operation receives load');
}

// The fixed mix keeps separate scenario metrics while making TARGET_TPS the
// total offered logical-operation rate, rather than the rate of each scenario.
const OPERATION_WEIGHTS = {
  normal_balanced_posting: 50,
  idempotent_duplicate_posting: 20,
  cross_ledger_transfer: 15,
  aggregate_event_ingestion: 15,
};

const normalRows = loadRows('normal_balanced_posting', [
  'debit_account_id',
  'credit_account_id',
]);
const duplicateRows = loadRows('idempotent_duplicate_posting', [
  'debit_account_id',
  'credit_account_id',
  'idempotency_key',
  'transaction_id',
]);
const transferRows = loadRows('cross_ledger_transfer', [
  'source_ledger_id',
  'source_account_id',
  'destination_ledger_id',
  'destination_account_id',
]);
const aggregateRows = loadRows('aggregate_event_ingestion', [
  'debit_account_id',
  'credit_account_id',
]);

const rates = allocateRates(TARGET_TPS, OPERATION_WEIGHTS);
const warmupCapacity = capacityFor(WARMUP_TPS, 0.6);
const normalCapacity = capacityFor(rates.normal_balanced_posting, 0.25);
const duplicateCapacity = capacityFor(rates.idempotent_duplicate_posting, 0.25);
const transferCapacity = capacityFor(
  rates.cross_ledger_transfer,
  POLL_TRANSFER_COMPLETION ? 2 : 0.25,
);
const aggregateCapacity = capacityFor(rates.aggregate_event_ingestion, 2);
const measuredStart = `${durationMilliseconds(WARMUP_DURATION)
  + durationMilliseconds(WARMUP_GRACEFUL_STOP)
  + durationMilliseconds(WARMUP_SETTLE_DURATION)}ms`;
const measuredDurationSeconds = durationMilliseconds(DURATION) / 1000;

const operationLatency = new Trend('ledger_operation_latency_ms', true);
const projectionLag = new Trend('ledger_projection_lag_ms', true);
const transferCompletionLag = new Trend('ledger_transfer_completion_lag_ms', true);
const logicalOperations = new Counter('ledger_logical_operations');
const successfulLogicalOperations = new Counter('ledger_successful_logical_operations');
const logicalErrorRate = new Rate('ledger_error_rate');
const projectionErrorRate = new Rate('ledger_projection_error_rate');
const transferCompletionErrorRate = new Rate('ledger_transfer_completion_error_rate');

export const options = {
  summaryTrendStats: ['avg', 'min', 'p(50)', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate',
      exec: 'warmup',
      rate: WARMUP_TPS,
      timeUnit: '1s',
      duration: WARMUP_DURATION,
      preAllocatedVUs: warmupCapacity.preAllocatedVUs,
      maxVUs: warmupCapacity.maxVUs,
      gracefulStop: WARMUP_GRACEFUL_STOP,
      tags: { phase: 'warmup' },
    },
    normal_balanced_posting: measuredScenario(
      'normalBalancedPosting',
      rates.normal_balanced_posting,
      normalCapacity,
    ),
    idempotent_duplicate_posting: measuredScenario(
      'idempotentDuplicatePosting',
      rates.idempotent_duplicate_posting,
      duplicateCapacity,
    ),
    cross_ledger_transfer: measuredScenario(
      'crossLedgerTransfer',
      rates.cross_ledger_transfer,
      transferCapacity,
    ),
    aggregate_event_ingestion: measuredScenario(
      'aggregateEventIngestion',
      rates.aggregate_event_ingestion,
      aggregateCapacity,
    ),
  },
  thresholds: {
    'ledger_operation_latency_ms{phase:measure}': [
      { threshold: 'p(99)<50', abortOnFail: false },
    ],
    'ledger_error_rate{phase:measure}': [
      { threshold: 'rate<0.01', abortOnFail: false },
    ],
    'ledger_operation_latency_ms{phase:measure,operation:normal_balanced_posting}': [
      { threshold: 'p(99)<50', abortOnFail: false },
    ],
    'ledger_operation_latency_ms{phase:measure,operation:idempotent_duplicate_posting}': [
      { threshold: 'p(99)<50', abortOnFail: false },
    ],
    'ledger_operation_latency_ms{phase:measure,operation:cross_ledger_transfer}': [
      { threshold: 'p(99)<50', abortOnFail: false },
    ],
    'ledger_operation_latency_ms{phase:measure,operation:aggregate_event_ingestion}': [
      { threshold: 'p(99)<50', abortOnFail: false },
    ],
    'ledger_error_rate{phase:measure,operation:normal_balanced_posting}': [
      { threshold: 'rate<0.01', abortOnFail: false },
    ],
    'ledger_error_rate{phase:measure,operation:idempotent_duplicate_posting}': [
      { threshold: 'rate<0.01', abortOnFail: false },
    ],
    'ledger_error_rate{phase:measure,operation:cross_ledger_transfer}': [
      { threshold: 'rate<0.01', abortOnFail: false },
    ],
    'ledger_error_rate{phase:measure,operation:aggregate_event_ingestion}': [
      { threshold: 'rate<0.01', abortOnFail: false },
    ],
    'ledger_logical_operations{phase:measure,operation:normal_balanced_posting}': [
      `count>=${Math.floor(rates.normal_balanced_posting * measuredDurationSeconds)}`,
    ],
    'ledger_logical_operations{phase:measure,operation:idempotent_duplicate_posting}': [
      `count>=${Math.floor(rates.idempotent_duplicate_posting * measuredDurationSeconds)}`,
    ],
    'ledger_logical_operations{phase:measure,operation:cross_ledger_transfer}': [
      `count>=${Math.floor(rates.cross_ledger_transfer * measuredDurationSeconds)}`,
    ],
    'ledger_logical_operations{phase:measure,operation:aggregate_event_ingestion}': [
      `count>=${Math.floor(rates.aggregate_event_ingestion * measuredDurationSeconds)}`,
    ],
    'dropped_iterations{scenario:normal_balanced_posting}': ['count==0'],
    'dropped_iterations{scenario:idempotent_duplicate_posting}': ['count==0'],
    'dropped_iterations{scenario:cross_ledger_transfer}': ['count==0'],
    'dropped_iterations{scenario:aggregate_event_ingestion}': ['count==0'],
  },
};

// A single warm-up executor avoids multiplying TARGET_TPS by four. Its
// deterministic permutation approximates the same 50/20/15/15 measured mix.
export function warmup() {
  const bucket = (13 + (exec.scenario.iterationInTest * 37)) % 100;
  if (bucket < 50) {
    runNormalBalancedPosting('warmup');
  } else if (bucket < 70) {
    runIdempotentDuplicatePosting('warmup');
  } else if (bucket < 85) {
    runCrossLedgerTransfer('warmup');
  } else {
    runAggregateEventIngestion('warmup');
  }
}

export function normalBalancedPosting() {
  runNormalBalancedPosting('measure');
}

export function idempotentDuplicatePosting() {
  runIdempotentDuplicatePosting('measure');
}

export function crossLedgerTransfer() {
  runCrossLedgerTransfer('measure');
}

export function aggregateEventIngestion() {
  runAggregateEventIngestion('measure');
}

function runNormalBalancedPosting(phase) {
  const operation = 'normal_balanced_posting';
  const tags = metricTags(phase, operation);
  const row = rowForIteration(normalRows);
  const response = postTransaction(row, tags);
  const body = responseJSON(response);
  const success = check(response, {
    'normal posting returns 201': (r) => r.status === 201,
    'normal posting returns a transaction': () => transactionShapeIsValid(body),
    'normal posting response is balanced': () => transactionIsBalanced(body),
  }, tags);

  recordLogicalOperation(phase, operation, response.timings.duration, success);
}

function runIdempotentDuplicatePosting(phase) {
  const operation = 'idempotent_duplicate_posting';
  const tags = metricTags(phase, operation);
  const row = rowForIteration(duplicateRows);
  const response = postTransaction(row, tags, {
    'Idempotency-Key': String(row.idempotency_key),
  });
  const body = responseJSON(response);
  const success = check(response, {
    'duplicate posting returns 200': (r) => r.status === 200,
    'duplicate posting returns the seeded transaction': () => (
      body !== null && body.id === String(row.transaction_id)
    ),
    'duplicate posting response is balanced': () => transactionIsBalanced(body),
  }, tags);

  recordLogicalOperation(phase, operation, response.timings.duration, success);
}

function runCrossLedgerTransfer(phase) {
  const operation = 'cross_ledger_transfer';
  const tags = metricTags(phase, operation);
  const row = rowForIteration(transferRows);
  const amount = amountFor(row);
  const idempotencyKey = [
    'k6', RUN_ID, phase, 'transfer', exec.vu.idInTest, exec.scenario.name,
    exec.scenario.iterationInTest,
  ].join('-');
  const request = {
    source_ledger_id: String(row.source_ledger_id),
    source_account_id: String(row.source_account_id),
    destination_ledger_id: String(row.destination_ledger_id),
    destination_account_id: String(row.destination_account_id),
    amount,
    idempotency_key: idempotencyKey,
  };
  const startedAt = Date.now();
  const response = http.post(
    `${BASE_URL}/transfers`,
    JSON.stringify(request),
    requestParameters(tags, 'primary'),
  );
  const body = responseJSON(response);
  let success = check(response, {
    'transfer enqueue returns 202': (r) => r.status === 202,
    'transfer enqueue returns an id': () => body !== null && nonEmptyString(body.id),
    'transfer enqueue is pending': () => body !== null && body.status === 'pending',
    'transfer response preserves boundaries': () => (
      body !== null
      && body.source_ledger_id === request.source_ledger_id
      && body.source_account_id === request.source_account_id
      && body.destination_ledger_id === request.destination_ledger_id
      && body.destination_account_id === request.destination_account_id
    ),
  }, tags);

  if (POLL_TRANSFER_COMPLETION && success) {
    const completed = waitForTransferCompletion(body.id, startedAt, tags);
    success = success && completed;
    if (phase === 'measure') {
      transferCompletionErrorRate.add(!completed, tags);
    }
  }

  const logicalDuration = POLL_TRANSFER_COMPLETION
    ? Date.now() - startedAt
    : response.timings.duration;
  recordLogicalOperation(phase, operation, logicalDuration, success);
}

function runAggregateEventIngestion(phase) {
  const operation = 'aggregate_event_ingestion';
  const tags = metricTags(phase, operation);
  const row = rowForIteration(aggregateRows);

  const startedAt = Date.now();
  const response = postTransaction(row, tags);
  const body = responseJSON(response);
  const postingOK = check(response, {
    'aggregate source posting returns 201': (r) => r.status === 201,
    'aggregate source returns a transaction': () => transactionShapeIsValid(body),
    'aggregate source response is balanced': () => transactionIsBalanced(body),
  }, tags);

  let projected = false;
  if (postingOK) {
    projected = waitForTransactionProjection(
      body.id,
      startedAt,
      tags,
      phase,
    );
  }
  const success = postingOK && projected;
  if (phase === 'measure') {
    projectionErrorRate.add(!projected, tags);
  }
  recordLogicalOperation(phase, operation, Date.now() - startedAt, success);
}

function postTransaction(row, tags, extraHeaders) {
  const amount = amountFor(row);
  const body = {
    entries: [
      {
        account_id: String(row.debit_account_id),
        debit: amount,
        credit: '0',
      },
      {
        account_id: String(row.credit_account_id),
        debit: '0',
        credit: amount,
      },
    ],
  };
  return http.post(
    `${BASE_URL}/transactions`,
    JSON.stringify(body),
    requestParameters(tags, 'primary', extraHeaders),
  );
}

function waitForTransactionProjection(transactionID, startedAt, tags, phase) {
  const waitMilliseconds = Math.floor(PROJECTION_TIMEOUT_SECONDS * 1000);
  const url = `${BASE_URL}/transactions/${encodeURIComponent(transactionID)}/projection-status?wait_ms=${waitMilliseconds}`;
  const response = http.get(url, requestParameters(tags, 'projection_wait'));
  const body = responseJSON(response);
  const projected = response.status === 200
    && body !== null
    && body.transaction_id === transactionID
    && body.consumer === 'daily-aggregates-v1'
    && nonEmptyString(body.event_id)
    && body.projected === true;
  if (projected && phase === 'measure') {
    projectionLag.add(Date.now() - startedAt, tags);
  }
  return projected;
}

function waitForTransferCompletion(transferID, startedAt, tags) {
  const waitMilliseconds = Math.floor(TRANSFER_TIMEOUT_SECONDS * 1000);
  const url = `${BASE_URL}/transfers/${encodeURIComponent(transferID)}?wait_ms=${waitMilliseconds}`;
  const response = http.get(url, requestParameters(tags, 'completion_wait'));
  const body = responseJSON(response);
  const completed = response.status === 200 && body !== null && body.status === 'completed';
  if (completed && tags.phase === 'measure') {
    transferCompletionLag.add(Date.now() - startedAt, tags);
  }
  return completed;
}

function recordLogicalOperation(phase, operation, duration, success) {
  if (phase !== 'measure') {
    return;
  }
  const tags = metricTags(phase, operation);
  logicalOperations.add(1, tags);
  if (success) {
    successfulLogicalOperations.add(1, tags);
  }
  logicalErrorRate.add(!success, tags);
  operationLatency.add(duration, tags);
}

function transactionShapeIsValid(body) {
  if (body === null || !nonEmptyString(body.id) || !Array.isArray(body.entries)) {
    return false;
  }
  if (body.entries.length !== 2) {
    return false;
  }
  return body.entries.every((entry) => (
    nonEmptyString(entry.id)
    && entry.transaction_id === body.id
    && nonEmptyString(entry.account_id)
  ));
}

function transactionIsBalanced(body) {
  if (body === null || !Array.isArray(body.entries) || body.entries.length < 2) {
    return false;
  }
  let debits = 0;
  let credits = 0;
  for (const entry of body.entries) {
    const debit = Number(entry.debit);
    const credit = Number(entry.credit);
    if (!Number.isFinite(debit) || !Number.isFinite(credit)) {
      return false;
    }
    debits += debit;
    credits += credit;
  }
  return Math.abs(debits - credits) < 0.000001;
}

function responseJSON(response) {
  try {
    return response.json();
  } catch (_) {
    return null;
  }
}

function requestParameters(tags, requestKind, extraHeaders) {
  return {
    headers: Object.assign(
      { 'Content-Type': 'application/json' },
      extraHeaders || {},
    ),
    tags: Object.assign({}, tags, { request_kind: requestKind }),
  };
}

function metricTags(phase, operation) {
  return { phase, operation };
}

function amountFor(row) {
  if (row.amount === undefined || row.amount === null || row.amount === '') {
    return String(DEFAULT_AMOUNT);
  }
  return String(row.amount);
}

function rowForIteration(rows) {
  return rows[exec.scenario.iterationInTest % rows.length];
}

function measuredScenario(execFunction, rate, capacity) {
  return {
    executor: 'constant-arrival-rate',
    exec: execFunction,
    startTime: measuredStart,
    rate,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: capacity.preAllocatedVUs,
    maxVUs: capacity.maxVUs,
    gracefulStop: GRACEFUL_STOP,
    tags: { phase: 'measure' },
  };
}

function loadRows(name, requiredFields) {
  return new SharedArray(`ledger-${name}`, function loadScenarioRows() {
    const document = JSON.parse(open(DATA_FILE));
    // The seeder writes a metadata-bearing document with arrays under
    // `scenarios`; accepting direct top-level arrays keeps hand-authored smoke
    // fixtures convenient and backwards compatible.
    const scenarios = document.scenarios || document;
    const rows = scenarios[name];
    if (!Array.isArray(rows) || rows.length === 0) {
      throw new Error(`${DATA_FILE}: ${name} must be a non-empty array`);
    }
    rows.forEach((row, index) => {
      requiredFields.forEach((field) => {
        if (row[field] === undefined || row[field] === null || row[field] === '') {
          throw new Error(`${DATA_FILE}: ${name}[${index}].${field} is required`);
        }
      });
    });
    return rows;
  });
}

function allocateRates(total, weights) {
  const names = Object.keys(weights);
  const ratesByName = {};
  const exactByName = {};
  const weightTotal = names.reduce((sum, name) => sum + weights[name], 0);
  let allocated = 0;

  names.forEach((name) => {
    const exact = total * (weights[name] / weightTotal);
    exactByName[name] = exact;
    ratesByName[name] = Math.max(1, Math.floor(exact));
    allocated += ratesByName[name];
  });

  while (allocated < total) {
    const name = names.reduce((best, candidate) => (
      exactByName[candidate] - ratesByName[candidate]
        > exactByName[best] - ratesByName[best]
        ? candidate
        : best
    ), names[0]);
    ratesByName[name] += 1;
    allocated += 1;
  }
  while (allocated > total) {
    const reducible = names.filter((name) => ratesByName[name] > 1);
    const name = reducible.reduce((best, candidate) => (
      ratesByName[candidate] - exactByName[candidate]
        > ratesByName[best] - exactByName[best]
        ? candidate
        : best
    ), reducible[0]);
    ratesByName[name] -= 1;
    allocated -= 1;
  }
  return ratesByName;
}

function capacityFor(rate, estimatedSeconds) {
  const configuredPreAllocated = optionalPositiveInteger('PRE_ALLOCATED_VUS');
  const configuredMax = optionalPositiveInteger('MAX_VUS');
  const preAllocatedVUs = configuredPreAllocated || Math.max(
    2,
    Math.ceil(rate * estimatedSeconds),
  );
  const maxVUs = configuredMax || Math.max(
    preAllocatedVUs + 5,
    preAllocatedVUs * 4,
  );
  if (maxVUs < preAllocatedVUs) {
    throw new Error('MAX_VUS must be greater than or equal to PRE_ALLOCATED_VUS');
  }
  return { preAllocatedVUs, maxVUs };
}

function positiveInteger(name, fallback) {
  const raw = __ENV[name];
  const value = raw === undefined || raw === '' ? fallback : Number(raw);
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive integer`);
  }
  return value;
}

function optionalPositiveInteger(name) {
  const raw = __ENV[name];
  if (raw === undefined || raw === '') {
    return null;
  }
  const value = Number(raw);
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${name} must be a positive integer`);
  }
  return value;
}

function positiveNumber(name, fallback) {
  const raw = __ENV[name];
  const value = raw === undefined || raw === '' ? fallback : Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number`);
  }
  return value;
}

function durationMilliseconds(value) {
  const compact = String(value).replace(/\s+/g, '');
  const pattern = /(\d+(?:\.\d+)?)(ms|s|m|h)/g;
  const multipliers = { ms: 1, s: 1000, m: 60000, h: 3600000 };
  let total = 0;
  let consumed = '';
  let match;
  while ((match = pattern.exec(compact)) !== null) {
    total += Number(match[1]) * multipliers[match[2]];
    consumed += match[0];
  }
  if (consumed !== compact || total <= 0) {
    throw new Error(`invalid duration: ${value}`);
  }
  return Math.ceil(total);
}

function nonEmptyString(value) {
  return typeof value === 'string' && value.length > 0;
}
