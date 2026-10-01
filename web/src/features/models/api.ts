import { apiRequest } from '../../shared/api/client'
import type {
  Candidate,
  CandidateTestRequest,
  CandidateTestResult,
  Model,
  ModelPatch,
  ModelsResponse,
  SaveSelection,
  CandidatesResponse,
  ModelTestJob,
  ModelTestJobRequest,
} from './types'

export interface ModelsApi {
  candidates(): Promise<CandidatesResponse>
  list(): Promise<ModelsResponse>
  save(models: SaveSelection[]): Promise<{ saved: number }>
  patch(id: number, patch: ModelPatch): Promise<Model>
  unblock(keyId: number, modelId: number): Promise<Model>
  delete(id: number): Promise<void>
  createTestJob(request: ModelTestJobRequest): Promise<ModelTestJob>
  getTestJob(id: string | number): Promise<ModelTestJob>
  cancelTestJob(id: string | number): Promise<ModelTestJob | void>
  testCandidate(request: CandidateTestRequest): Promise<CandidateTestResult>
}

export const modelsApi: ModelsApi = {
  candidates() {
    return apiRequest('/admin/api/models/candidates')
  },
  list() {
    return apiRequest('/admin/api/models')
  },
  save(models) {
    return apiRequest('/admin/api/models', { method: 'POST', body: { models } })
  },
  patch(id, patch) {
    return apiRequest(`/admin/api/models/${id}`, { method: 'PATCH', body: patch })
  },
  unblock(keyId, modelId) {
    return apiRequest(`/admin/api/key-model-blocks/${keyId}/${modelId}`, { method: 'DELETE' })
  },
  delete(id) {
    return apiRequest(`/admin/api/models/${id}`, { method: 'DELETE' })
  },
  createTestJob(request) {
    return apiRequest('/admin/api/model-test-jobs', { method: 'POST', body: request })
  },
  getTestJob(id) {
    return apiRequest(`/admin/api/model-test-jobs/${encodeURIComponent(String(id))}`)
  },
  cancelTestJob(id) {
    return apiRequest(`/admin/api/model-test-jobs/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
  },
  testCandidate(request) {
    return apiRequest('/admin/api/models/candidates/test', { method: 'POST', body: request })
  },
}

export type {
  Candidate,
  CandidateTestRequest,
  CandidateTestResult,
  Model,
  ModelPatch,
  ModelTestJob,
  ModelTestJobRequest,
  SaveSelection,
}
