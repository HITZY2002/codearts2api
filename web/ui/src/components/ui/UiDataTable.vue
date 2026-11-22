<script setup>
// 轻量数据表：columns 定义与 naive-ui 同形（title/key/width/minWidth/ellipsis/
// align/fixed/render），render 返回 vnode，页面列定义可机械迁移。
// pagination: { page, pageSize, itemCount?, local?, onUpdatePage, onUpdatePageSize? }
import { computed } from 'vue'
import UiPagination from './UiPagination.vue'
import UiSpinner from './UiSpinner.vue'

const props = defineProps({
  columns: { type: Array, required: true },
  data: { type: Array, default: () => [] },
  loading: Boolean,
  rowKey: { type: Function, default: (row, index) => index },
  scrollX: { type: [Number, String], default: 0 },
  pagination: { type: Object, default: null }
})

// render vnode 需要作为子节点而非文本插入，包一层透传组件。
const Cell = props => props.node
Cell.props = ['node']

const rows = computed(() => {
  if (props.pagination?.local) {
    const start = (props.pagination.page - 1) * props.pagination.pageSize
    return props.data.slice(start, start + props.pagination.pageSize)
  }
  return props.data
})

const itemCount = computed(() =>
  props.pagination ? (props.pagination.itemCount ?? props.data.length) : 0
)

function colStyle(col) {
  const w = col.width ?? col.minWidth
  return w != null ? { width: `${w}px` } : null
}
function cellClass(col) {
  return {
    right: col.align === 'right',
    wrap: col.className === 'wrap-cell' || (col.className || '').includes('wrap-cell'),
    'is-fixed-right': col.fixed === 'right'
  }
}
function onTitle(col, row) {
  if (col.render) return undefined
  return String(row[col.key] ?? '')
}

const tableStyle = computed(() => (props.scrollX ? { minWidth: `${props.scrollX}px` } : null))
const showPagination = computed(() => Boolean(props.pagination))
</script>

<template>
  <div class="u-table" :class="{ 'is-loading': loading }">
    <div class="u-table-scroll">
      <table :style="tableStyle">
        <colgroup>
          <col v-for="col in columns" :key="col.key" :style="colStyle(col)" />
        </colgroup>
        <thead>
          <tr>
            <th
              v-for="col in columns" :key="col.key"
              :class="{ right: col.align === 'right', 'is-fixed-right': col.fixed === 'right' }"
              :title="col.title"
            >{{ col.title }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, index) in rows" :key="rowKey(row, index)">
            <td
              v-for="col in columns" :key="col.key"
              :class="cellClass(col)" :title="onTitle(col, row)"
            >
              <Cell v-if="col.render" :node="col.render(row)" />
              <template v-else>{{ row[col.key] ?? '' }}</template>
            </td>
          </tr>
          <tr v-if="!rows.length">
            <td :colspan="columns.length" class="u-table-empty">{{ loading ? '加载中…' : '暂无数据' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <UiPagination
      v-if="showPagination"
      :page="pagination.page"
      :page-size="pagination.pageSize"
      :item-count="itemCount"
      @update:page="pagination.onUpdatePage?.($event)"
      @update:page-size="pagination.onUpdatePageSize?.($event)"
    />
    <Transition name="fade">
      <div v-if="loading" class="table-loading">
        <UiSpinner :size="22" />
      </div>
    </Transition>
  </div>
</template>
